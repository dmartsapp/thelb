package director

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"hash/fnv"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/check"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/ipvs"
	"github.com/dmartsapp/thelb/internal/wire"
)

// Dataplane is the kernel state of the director holding the service VIPs: the
// VIPs on their interfaces, announced; an IPVS service for each service, its
// backends as direct-routing destinations; each backend's address pinned to
// its MAC address.
type Dataplane interface {
	// Apply makes the kernel match the plan - adding, changing and removing
	// only what is theLB's.
	Apply(Plan) error
	// Stats are IPVS's counters, by DestKey.
	Stats() map[string]ipvs.Stats
	// Clear removes everything Apply added, and restores the kernel parameters
	// it changed.
	Clear()
	// Probe asks the network who has addr: a MAC address, or "".
	Probe(ctx context.Context, addr netip.Addr) (string, error)
	// ProbeAll asks the network, on each VIP's interface, who has it - all at
	// once: a MAC address for each VIP another machine answers for.
	ProbeAll(ctx context.Context, vips []PlanVIP) (map[netip.Addr]string, error)
	// Leftovers removes the plan's IPVS services and pinned backends where the
	// kernel has them and this data plane did not add them: what a crash left,
	// on a director that does not lead.
	Leftovers(Plan) error
	// Sync runs IPVS's connection-synchronisation daemons as s says - a backup
	// on every member, a master on the leader - and stops those it does not say,
	// a master an earlier run left among them. SyncState{} stops both.
	Sync(s SyncState) error
}

// SyncState is the IPVS connection synchronisation a director runs: the leader
// sends its connections, every member takes in the leader's, so a director that
// takes the VIPs over carries on with connections already in progress.
type SyncState struct {
	Interface string // where the directors meet: the control VIP's interface
	SyncID    uint32 // the cluster's, so two clusters on one network do not mix
	Master    bool
	Backup    bool
}

// syncID is a cluster's IPVS sync ID, from its ID: 1 to 255 (a backup with ID 0
// would take in every cluster's connections).
func syncID(clusterID string) uint32 {
	h := sha256.Sum256([]byte(clusterID))
	return 1 + uint32(h[0])%255
}

// syncDaemons sets this director's IPVS connection synchronisation: a backup
// while it is a member, and a master too while it leads; stop ends both. Whether
// it leads is read here, under syncMu, so of two calls racing a change of
// leadership the last one is right.
func (s *session) syncDaemons(stop bool) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	master := s.leading() != nil
	want := SyncState{}
	if !stop {
		s.mu.Lock()
		member := s.member
		s.mu.Unlock()
		if !member {
			return // not admitted yet: waitMember starts the backup once it is
		}
		var id string
		s.fsm.Read(func(st *cluster.State) {
			if st.Cluster != nil {
				id = st.Cluster.ID
			}
		})
		if id == "" {
			return // not a cluster's member yet
		}
		want = SyncState{Interface: s.n.addr.Interface, SyncID: syncID(id), Master: master, Backup: true}
	}
	err := s.dp().Sync(want)
	s.mu.Lock()
	said := s.syncErr
	if err != nil {
		s.syncErr = err.Error()
	} else {
		s.syncErr = ""
	}
	s.mu.Unlock()
	if err != nil && err.Error() != said {
		s.log.Warn("IPVS connection synchronisation failed: connections in progress would not survive a failover", "error", err)
	}
}

// Plan is what the kernel is to hold.
type Plan struct {
	VIPs     []PlanVIP
	Services []PlanService
	Pins     []PlanPin
	// Pools are the cluster's pools, for Leftovers only: an IPVS service on a
	// pool's address is theLB's, though the cluster no longer has it - a service
	// removed while this director was down.
	Pools []cluster.Pool
	// Lease is how long the kernel keeps a service VIP after its last renewal:
	// the cluster's VIP lease.
	Lease time.Duration
}

// PlanVIP is a service VIP, on its pool's interface.
type PlanVIP struct {
	Addr      netip.Addr
	Interface string
}

// PlanService is an IPVS service.
type PlanService struct {
	ipvs.Service
	Dests []ipvs.Dest
}

// PlanPin is a backend's address pinned to its MAC address, on the interface
// that reaches it.
type PlanPin struct {
	Addr      netip.Addr
	MAC       string
	Interface string
}

// DestKey names a destination of a service.
func DestKey(s ipvs.Service, d netip.AddrPort) string { return s.Key() + " " + d.String() }

// Timing of the data plane.
var (
	applyEvery  = time.Second      // the data plane, when nothing changed
	repairEvery = 10 * time.Second // the control VIP checked, and put back
	// stallAfter is how long a loop may go without going round before
	// systemd's watchdog is told no more.
	stallAfter = 30 * time.Second
)

// backendLive is a backend's last heartbeat, as the leader holds it.
type backendLive struct {
	at      time.Time
	checks  map[string]bool
	holding map[string]bool // the VIPs it holds: a set, looked up for every assignment
	noNFT   bool            // its kernel has no nftables
	leaving bool
}

// prober is a director's check of a backend's application: the leader's own,
// or one it gave this director.
type prober struct {
	spec   string
	target netip.AddrPort
	stop   context.CancelFunc
	mu     sync.Mutex
	state  check.State
	seen   bool // checked at least once
}

// startProber checks spec at target every check.Every until ctx ends, from
// healthy: where the leader's view of it stands.
func startProber(ctx context.Context, spec string, target netip.AddrPort, healthy bool) *prober {
	cctx, stop := context.WithCancel(ctx)
	pr := &prober{spec: spec, target: target, stop: stop, state: check.State{Healthy: healthy}}
	go func() {
		t := time.NewTicker(check.Every)
		defer t.Stop()
		for {
			err := check.Probe(cctx, spec, target, check.Timeout)
			if cctx.Err() != nil {
				return
			}
			pr.mu.Lock()
			pr.state.Observe(err)
			pr.seen = true
			pr.mu.Unlock()
			select {
			case <-cctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return pr
}

// result is the prober's result, as a check-in reports it.
func (p *prober) result() api.ProbeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return api.ProbeResult{Healthy: p.state.Healthy, Seen: p.seen}
}

// remoteCheck is a member's last result of a check the leader gave it.
type remoteCheck struct {
	by  string
	at  time.Time
	res api.ProbeResult
}

func (p *prober) healthy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.Healthy
}

// plane is the leader's data plane, for one leadership.
type plane struct {
	s       *session
	mu      sync.Mutex
	probers map[string]*prober              // the leader's own, by what it checks - its spec and target: one for every assignment of that offer
	checkOf map[string]string               // by assignment key: its check's key
	remote  map[string]remoteCheck          // by check key: a member's last result
	given   map[string]map[string]api.Probe // by director, by check key: the checks the leader gives it
	checkBy map[string]string               // by check key: who makes it now
	weights map[string]int                  // by assignment key: what IPVS has
	since   time.Time                       // leadership began: beats before it are unknown

	held      map[netip.Addr]bool      // the service VIPs the kernel holds, as applied
	freeSince map[netip.Addr]time.Time // a VIP to take: when a look first found no machine answering for it
	answered  map[netip.Addr]time.Time // a VIP to take: since when another machine answers for it
	said      map[netip.Addr]string    // the last warning about a VIP to take
	probeErr  string
}

func (s *session) dp() Dataplane { return s.n.plane }

// serve runs the data plane while this director leads: it holds the service
// VIPs, programs IPVS, checks the backends, and writes their health.
func (s *session) serve(ctx context.Context) {
	p := &plane{s: s, probers: map[string]*prober{}, checkOf: map[string]string{}, weights: map[string]int{}, since: time.Now(),
		remote: map[string]remoteCheck{}, given: map[string]map[string]api.Probe{}, checkBy: map[string]string{},
		held: map[netip.Addr]bool{}, freeSince: map[netip.Addr]time.Time{}, answered: map[netip.Addr]time.Time{}, said: map[netip.Addr]string{}}
	s.mu.Lock()
	s.plane = p
	s.mu.Unlock()
	defer func() {
		p.mu.Lock()
		for _, pr := range p.probers {
			pr.stop()
		}
		p.mu.Unlock()
		s.dp().Clear()
		s.mu.Lock()
		s.plane = nil
		s.mu.Unlock()
	}()
	defer s.n.alive.Done("data plane")
	var lastErr, repairErr string
	var slowSaid time.Time
	repaired := time.Now()
	s.syncDaemons(false) // the master too, now that it leads
	defer func() {
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if !closing {
			s.syncDaemons(false) // a follower again: the backup only
		}
	}()
	for {
		s.n.alive.Tick("data plane", stallAfter)
		changed := s.fsm.Changed()
		if time.Since(repaired) >= repairEvery {
			repaired = time.Now()
			if fixed, err := s.n.net.Repair(); err != nil {
				if err.Error() != repairErr {
					s.log.Error("checking the control VIP failed", "vip", s.n.cfg.ControlVIP, "error", err)
				}
				repairErr = err.Error()
			} else {
				repairErr = ""
				if fixed {
					s.log.Warn("drift repaired: the control VIP was removed from its interface; added and announced again", "vip", s.n.cfg.ControlVIP, "interface", s.n.addr.Interface)
				}
			}
		}
		began := time.Now()
		err := p.step(ctx)
		if took := time.Since(began); took > 2*time.Second && time.Since(slowSaid) > time.Minute {
			slowSaid = time.Now()
			s.log.Warn("building and applying the data plane took longer than it should", "took", took.Round(time.Millisecond))
		}
		if err != nil {
			if err.Error() != lastErr {
				s.log.Error("programming the data plane failed", "error", err)
			}
			lastErr = err.Error()
		} else if lastErr != "" {
			s.log.OK("the data plane is programmed again")
			lastErr = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(applyEvery):
		}
	}
}

// step builds the plan from the state and the backends' heartbeats, and applies
// it.
func (p *plane) step(ctx context.Context) error {
	s := p.s
	var services []cluster.Service
	pools := map[string]cluster.Pool{}
	var assignments []cluster.Assignment
	nodes := map[string]cluster.Node{}
	offers := map[string][]cluster.Offer{}
	s.fsm.Read(func(st *cluster.State) {
		for _, v := range st.Services {
			services = append(services, *v)
		}
		for k, v := range st.Pools {
			pools[k] = *v
		}
		for _, a := range st.Assignments {
			assignments = append(assignments, *a)
		}
		for k, n := range st.Nodes {
			nodes[k] = *n
		}
		for k, o := range st.Offers {
			offers[k] = o.Offers
		}
	})
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	tunnel := map[string]bool{}
	for _, v := range services {
		tunnel[v.Name] = v.Tunnel()
	}
	checkIns := s.checkInsCopy()
	s.mu.Lock()
	beats := map[string]backendLive{}
	for k, v := range s.backends {
		beats[k] = v
	}
	s.mu.Unlock()

	var plan Plan
	seenVIP := map[netip.Addr]bool{}
	// The VIPs, in order, and their interfaces: each held only once a backend
	// serves one of its services - after IPVS has it.
	var vips []PlanVIP
	serving := map[netip.Addr]bool{}
	// Each service's place in plan.Services - an index, not a pointer: the
	// slice moves as it grows.
	byKey := map[string]int{}
	// Each pool's interface, found once: finding one reads every address.
	ifcOf := map[string]string{}
	for _, v := range services {
		vip, err := netip.ParseAddr(v.VIP)
		if err != nil {
			continue
		}
		pool := pools[v.Pool]
		if !seenVIP[vip] {
			seenVIP[vip] = true
			ifname, ok := ifcOf[pool.Name]
			if !ok {
				first, _ := netip.ParseAddr(pool.First)
				if ifc, err := s.n.local(first, pool.Interface); err != nil {
					s.log.Warn("this director cannot hold a pool's VIPs", "pool", pool.Name, "error", err)
				} else {
					ifname = ifc.Interface
				}
				ifcOf[pool.Name] = ifname
			}
			if ifname == "" {
				continue
			}
			vips = append(vips, PlanVIP{Addr: vip, Interface: ifname})
		}
		proto, _ := ipvs.ProtoNumber(v.Proto)
		plan.Services = append(plan.Services, PlanService{Service: ipvs.Service{Addr: vip, Protocol: proto, Port: uint16(v.Port), Scheduler: v.Scheduler}})
		byKey[v.Name] = len(plan.Services) - 1
	}

	wantProbers := map[string]bool{}
	wantAssigned := map[string]bool{}
	pinned := map[netip.Addr]bool{}
	checkers := p.checkers(checkIns, nodes)
	given := map[string]map[string]api.Probe{}
	for _, a := range assignments {
		i, found := byKey[a.Service]
		n, ok := nodes[a.Node]
		if !found || !ok {
			continue
		}
		ps := &plan.Services[i]
		addr, err := netip.ParseAddr(n.Address)
		if err != nil {
			continue
		}
		key := cluster.AssignmentKey(a.Service, a.Node)
		spec := "tcp"
		for _, o := range offers[a.Node] {
			if o.Proto == a.Proto && o.Port == a.Port {
				spec = o.Check
			}
		}
		// One check of a backend's application, however many services it serves.
		target := netip.AddrPortFrom(addr, uint16(a.Port))
		probeKey := spec + " " + target.String()
		wantProbers[probeKey], wantAssigned[key] = true, true
		dirOK := p.check(ctx, probeKey, spec, target, a.Healthy, checkers, wantProbers, given)
		p.mu.Lock()
		p.checkOf[key] = probeKey
		p.mu.Unlock()
		ready := p.ready(a, n, ps.Addr, checkIns, beats, dirOK)
		w := 0
		if ready {
			w = a.Weight
		}
		if w > 0 {
			serving[ps.Addr] = true
		}
		// Direct routing never changes the port: the destination is the backend
		// at the service's port, which the backend maps to its application's.
		ps.Dests = append(ps.Dests, ipvs.Dest{Addr: addr, Port: ps.Port, Weight: uint32(w), Tunnel: tunnel[a.Service]})
		p.mu.Lock()
		p.weights[key] = w
		p.mu.Unlock()
		if ready != a.Healthy {
			if err := s.apply(cluster.TypeHealth, s.n.cfg.Name, cluster.Health{Service: a.Service, Node: a.Node, Healthy: ready}); err == nil {
				if ready {
					s.log.OK("backend serving", "service", a.Service, "node", a.Node, "at", netip.AddrPortFrom(addr, uint16(a.Port)), "weight", a.Weight)
				} else {
					s.log.Warn("backend not serving: weight 0", "service", a.Service, "node", a.Node, "at", netip.AddrPortFrom(addr, uint16(a.Port)), "reason", p.why(a, n, ps.Addr, checkIns, beats, dirOK))
				}
			}
		}
		// This director's own backend gets its share through the local stack
		// (IPVS's local-node forwarding): nothing to pin.
		if !pinned[addr] && addr != s.n.addr.Prefix.Addr() {
			pinned[addr] = true
			if ifc, err := s.n.local(addr, ""); err == nil && n.MAC != "" {
				plan.Pins = append(plan.Pins, PlanPin{Addr: addr, MAC: n.MAC, Interface: ifc.Interface})
			}
		}
	}
	var serve []PlanVIP
	for _, v := range vips {
		if serving[v.Addr] {
			serve = append(serve, v)
		}
	}
	plan.VIPs = p.claim(ctx, serve)
	plan.Lease = s.vipLease()
	p.mu.Lock()
	p.given = given
	for key, pr := range p.probers {
		if !wantProbers[key] {
			pr.stop()
			delete(p.probers, key)
		}
	}
	for key := range p.weights {
		if !wantAssigned[key] {
			delete(p.weights, key)
			delete(p.checkOf, key)
		}
	}
	p.mu.Unlock()
	err := s.dp().Apply(plan)
	held := map[netip.Addr]bool{}
	for _, v := range plan.VIPs {
		if err == nil || p.held[v.Addr] {
			held[v.Addr] = true
		}
	}
	p.held = held
	return err
}

// claim is the service VIPs of want this leader may hold: those it holds
// already, and each other one once it is no more - two looks, a second or more
// apart, found no machine answering for it. A leader never takes a VIP another
// machine still answers for: a leader killed leaves its VIPs on its interface
// - and its IPVS forwarding to the backends - until their lease lapses.
func (p *plane) claim(ctx context.Context, want []PlanVIP) []PlanVIP {
	s := p.s
	var out, pending []PlanVIP
	wanted := map[netip.Addr]bool{}
	for _, v := range want {
		wanted[v.Addr] = true
		if p.held[v.Addr] {
			out = append(out, v)
		} else {
			pending = append(pending, v)
		}
	}
	for a := range p.freeSince {
		if !wanted[a] || p.held[a] {
			delete(p.freeSince, a)
		}
	}
	for a := range p.answered {
		if !wanted[a] || p.held[a] {
			delete(p.answered, a)
			delete(p.said, a)
		}
	}
	if len(pending) == 0 {
		return out
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	answers, err := s.dp().ProbeAll(pctx, pending)
	cancel()
	if err != nil {
		if err.Error() != p.probeErr {
			s.log.Warn("asking who has the service VIPs to take failed: they wait", "error", err)
		}
		p.probeErr = err.Error()
		return out
	}
	p.probeErr = ""
	now := time.Now()
	lease := s.vipLease()
	for _, v := range pending {
		if who := answers[v.Addr]; who != "" {
			delete(p.freeSince, v.Addr)
			since, ok := p.answered[v.Addr]
			if !ok {
				since = now
				p.answered[v.Addr] = now
			}
			why := fmt.Sprintf("%s still answers for it: this leader takes it once it is no more - within the %s lease, if a theLB that stopped left it", who, lease)
			if now.Sub(since) > lease+leaseSlack {
				why = fmt.Sprintf("%s still answers for it, past the %s lease: set there by hand, or by a theLB alive - remove it there (ip address del)", who, lease)
			}
			if p.said[v.Addr] != why {
				p.said[v.Addr] = why
				s.log.Warn("another machine answers for a service VIP: this leader waits", "vip", v.Addr, "reason", why)
			}
			continue
		}
		delete(p.answered, v.Addr)
		first, ok := p.freeSince[v.Addr]
		switch {
		case !ok:
			p.freeSince[v.Addr] = now
		case now.Sub(first) >= time.Second:
			delete(p.freeSince, v.Addr)
			if p.said[v.Addr] != "" {
				s.log.OK("the service VIP is no more: taking it", "vip", v.Addr)
				delete(p.said, v.Addr)
			}
			out = append(out, v)
		}
	}
	return out
}

// prober is the leader's own check of an application, started on first need;
// a new leader starts it as healthy as the state says the assignment was.
func (p *plane) prober(ctx context.Context, key, spec string, target netip.AddrPort, healthy bool) *prober {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr, ok := p.probers[key]; ok && pr.spec == spec && pr.target == target {
		return pr
	} else if ok {
		pr.stop()
	}
	pr := startProber(ctx, spec, target, healthy)
	p.probers[key] = pr
	return pr
}

// checkers is who may check backends' applications for the leader: itself, and
// every director that checked in within liveFor - sorted.
func (p *plane) checkers(checkIns map[string]checkIn, nodes map[string]cluster.Node) []string {
	out := []string{p.s.n.cfg.Name}
	for name, ci := range checkIns {
		if n, ok := nodes[name]; ok && n.Role == "director" && name != p.s.n.cfg.Name && !ci.Gone && time.Since(ci.At) < liveFor {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// checker is the director that checks an application, of those that may:
// rendezvous hashing, so that a director coming or going moves only the checks
// that were, or become, its own.
func checker(key string, checkers []string) string {
	best, top := "", uint64(0)
	for _, d := range checkers {
		h := fnv.New64a()
		_, _ = h.Write([]byte(key + "\x00" + d))
		if v := h.Sum64(); best == "" || v > top {
			best, top = d, v
		}
	}
	return best
}

// check is whether the director's check of an application passes: its
// director's last result - once it has one, and fresh - or the leader's own,
// meanwhile. A check given to another director is put in given, for its next
// check-in; one the leader makes itself in local.
func (p *plane) check(ctx context.Context, key, spec string, target netip.AddrPort, healthy bool, checkers []string, local map[string]bool, given map[string]map[string]api.Probe) bool {
	self := p.s.n.cfg.Name
	p.mu.Lock()
	if pr, ok := p.probers[key]; ok {
		healthy = pr.healthy() // where the leader's view stands now
	}
	by := checker(key, checkers)
	if by != self {
		if given[by] == nil {
			given[by] = map[string]api.Probe{}
		}
		given[by][key] = api.Probe{Key: key, Spec: spec, Target: target.String(), Healthy: healthy}
		if r, ok := p.remote[key]; ok && r.by == by && r.res.Seen && time.Since(r.at) < liveFor {
			p.checkBy[key] = by
			p.mu.Unlock()
			return r.res.Healthy
		}
	}
	p.checkBy[key] = self
	p.mu.Unlock()
	local[key] = true
	return p.prober(ctx, key, spec, target, healthy).healthy()
}

// report takes a member's results, with its check-in, and is the checks the
// leader gives it.
func (p *plane) report(name string, results map[string]api.ProbeResult) []api.Probe {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for key, r := range results {
		p.remote[key] = remoteCheck{by: name, at: now, res: r}
	}
	out := make([]api.Probe, 0, len(p.given[name]))
	for _, pb := range p.given[name] {
		out = append(out, pb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ready is whether an assignment serves: the backend's heartbeat lease is
// current, its own check passes, it holds the VIP, and the leader's check
// passes. Until a new leader hears from a backend, it goes by the state.
func (p *plane) ready(a cluster.Assignment, n cluster.Node, vip netip.Addr, checkIns map[string]checkIn, beats map[string]backendLive, dirOK bool) bool {
	return p.why(a, n, vip, checkIns, beats, dirOK) == ""
}

func (p *plane) why(a cluster.Assignment, n cluster.Node, vip netip.Addr, checkIns map[string]checkIn, beats map[string]backendLive, dirOK bool) string {
	ci, ok := checkIns[n.Name]
	if !ok || ci.RaftID != n.RaftID || time.Since(ci.At) > backendLease {
		return "no heartbeat"
	}
	b, beat := beats[n.Name]
	if beat && b.leaving {
		return "it is stopping"
	}
	if beat && b.at.After(p.since) {
		if !b.checks[fmt.Sprintf("%s/%d", a.Proto, a.Port)] {
			return "its own check fails"
		}
		if !b.holding[vip.String()] {
			return "it does not hold the VIP yet"
		}
	} else if !a.Healthy {
		return "no heartbeat since this director began to lead"
	}
	if !dirOK {
		return "the director's check fails"
	}
	return ""
}

// liveNow is what only the leader knows, for status.
func (s *session) liveNow(brief bool) ([]api.CheckIn, []api.Live, []api.Own) {
	var cis []api.CheckIn
	for _, ci := range s.checkInsCopy() {
		cis = append(cis, api.CheckIn(ci))
	}
	s.mu.Lock()
	p := s.plane
	beats := map[string]backendLive{}
	for k, v := range s.backends {
		beats[k] = v
	}
	s.mu.Unlock()
	if p == nil {
		return cis, nil, nil
	}
	var own []api.Own
	for _, node := range sortedNames(beats) {
		b := beats[node]
		if !b.at.After(p.since) {
			continue
		}
		for _, key := range sortedNames(b.checks) {
			w := "fail"
			if b.checks[key] {
				w = "ok"
			}
			own = append(own, api.Own{Node: node, Offer: key, Check: w})
		}
	}
	var stats map[string]ipvs.Stats
	if !brief {
		stats = s.dp().Stats()
	}
	var live []api.Live
	var assignments []cluster.Assignment
	vips := map[string]string{}
	nodes := map[string]cluster.Node{}
	services := map[string]cluster.Service{}
	s.fsm.Read(func(st *cluster.State) {
		for _, a := range st.Assignments {
			assignments = append(assignments, *a)
		}
		for k, v := range st.Services {
			vips[k] = v.VIP
			services[k] = *v
		}
		for k, n := range st.Nodes {
			nodes[k] = *n
		}
	})
	for _, a := range assignments {
		key := cluster.AssignmentKey(a.Service, a.Node)
		l := api.Live{Service: a.Service, Node: a.Node, Own: "-", Director: "-"}
		if b, ok := beats[a.Node]; ok && b.at.After(p.since) {
			l.Own = "fail"
			if b.checks[fmt.Sprintf("%s/%d", a.Proto, a.Port)] {
				l.Own = "ok"
			}
		}
		p.mu.Lock()
		ck := p.checkOf[key]
		l.By = p.checkBy[ck]
		if r, ok := p.remote[ck]; ok && l.By != s.n.cfg.Name && r.by == l.By {
			l.Director = map[bool]string{true: "ok", false: "fail"}[r.res.Healthy]
		} else if pr := p.probers[ck]; pr != nil {
			pr.mu.Lock()
			if pr.seen || pr.state.Healthy {
				l.Director = pr.state.Word()
			}
			pr.mu.Unlock()
		}
		l.Weight = p.weights[key]
		p.mu.Unlock()
		v := services[a.Service]
		vip, _ := netip.ParseAddr(vips[a.Service])
		proto, _ := ipvs.ProtoNumber(v.Proto)
		addr, _ := netip.ParseAddr(nodes[a.Node].Address)
		st := stats[DestKey(ipvs.Service{Addr: vip, Protocol: proto, Port: uint16(v.Port)}, netip.AddrPortFrom(addr, uint16(v.Port)))]
		l.Conns, l.Packets, l.Bytes = st.Conns, st.Packets, st.Bytes
		live = append(live, l)
	}
	return cis, live, own
}

// serveBackend takes a backend's heartbeats, while this director leads, and
// answers each with what the backend is to serve, and the offers the cluster
// holds for it.
func (s *session) serveBackend(c *tls.Conn, w *wire.Conn, name string, req api.Request) {
	for {
		if s.leading() == nil {
			_ = w.Send(api.Response{V: wire.Version, Error: errNotLeader.Error()})
			return
		}
		b := req.Beat
		if b == nil {
			_ = w.Send(api.Response{V: wire.Version, Error: "a heartbeat without a beat"})
			return
		}
		var n *cluster.Node
		orders := api.Orders{Offers: []cluster.Offer{}}
		s.fsm.Read(func(st *cluster.State) {
			if p := st.Nodes[name]; p != nil {
				cp := *p
				n = &cp
			}
			if o := st.Offers[name]; o != nil {
				orders.Offers = append(orders.Offers, o.Offers...)
			}
			for _, a := range st.Assignments {
				if a.Node != name {
					continue
				}
				if v := st.Services[a.Service]; v != nil {
					orders.Assignments = append(orders.Assignments, api.Assignment{Service: v.Name, VIP: v.VIP, Proto: a.Proto, Port: v.Port, AppPort: a.Port, Tunnel: v.Tunnel()})
				}
			}
		})
		if n == nil {
			_ = w.Send(api.Response{V: wire.Version, OK: true, Orders: &api.Orders{Removed: true}})
			return
		}
		if n.RaftID != b.Member {
			_ = w.Send(api.Response{V: wire.Version, Error: "another member identity holds this backend's name"})
			return
		}
		now := time.Now()
		s.mu.Lock()
		s.checkIns[name] = checkIn{Name: name, RaftID: b.Member, At: now, Version: b.Version}
		holding := make(map[string]bool, len(b.Holding))
		for _, v := range b.Holding {
			holding[v] = true
		}
		s.backends[name] = backendLive{at: now, checks: b.Checks, holding: holding, leaving: b.Leaving, noNFT: b.NoNFT}
		s.mu.Unlock()
		if b.Leaving {
			s.log.OK("backend stopping: draining it", "node", name)
			_ = w.Send(api.Response{V: wire.Version, OK: true, Orders: &api.Orders{}})
			return
		}
		sort.Slice(orders.Assignments, func(i, j int) bool { return orders.Assignments[i].Service < orders.Assignments[j].Service })
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if w.Send(api.Response{V: wire.Version, OK: true, Orders: &orders}) != nil {
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(backendLease))
		req = api.Request{}
		if w.Recv(&req) != nil {
			return
		}
	}
}

// nothing is a data plane that holds nothing, for a director that cannot
// program one.
type nothing struct{}

func (nothing) Apply(p Plan) error {
	if len(p.Services) > 0 {
		return fmt.Errorf("this director has no data plane")
	}
	return nil
}
func (nothing) Stats() map[string]ipvs.Stats                      { return nil }
func (nothing) Clear()                                            {}
func (nothing) Probe(context.Context, netip.Addr) (string, error) { return "", nil }
func (nothing) Leftovers(Plan) error                              { return nil }
func (nothing) Sync(SyncState) error                              { return nil }
func (nothing) ProbeAll(context.Context, []PlanVIP) (map[netip.Addr]string, error) {
	return nil, nil
}

// known is what the cluster's state says a holder of the VIPs programs - its
// services and pinned backends, whatever their health - for Leftovers.
func (s *session) known() Plan {
	var plan Plan
	s.fsm.Read(func(st *cluster.State) {
		for _, p := range st.Pools {
			plan.Pools = append(plan.Pools, *p)
		}
		for _, v := range st.Services {
			vip, err := netip.ParseAddr(v.VIP)
			if err != nil {
				continue
			}
			proto, _ := ipvs.ProtoNumber(v.Proto)
			plan.Services = append(plan.Services, PlanService{Service: ipvs.Service{Addr: vip, Protocol: proto, Port: uint16(v.Port), Scheduler: v.Scheduler}})
		}
		for _, n := range st.Nodes {
			if n.Role != "backend" {
				continue
			}
			addr, err := netip.ParseAddr(n.Address)
			if err != nil || addr == s.n.addr.Prefix.Addr() {
				continue
			}
			if ifc, err := s.n.local(addr, ""); err == nil {
				plan.Pins = append(plan.Pins, PlanPin{Addr: addr, MAC: n.MAC, Interface: ifc.Interface})
			}
		}
	})
	return plan
}
