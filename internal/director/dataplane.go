package director

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net/netip"
	"slices"
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
	holding []string
	leaving bool
}

// prober is the leader's own check of one assignment.
type prober struct {
	spec   string
	target netip.AddrPort
	stop   context.CancelFunc
	mu     sync.Mutex
	state  check.State
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
	probers map[string]*prober // by assignment key
	weights map[string]int     // by assignment key: what IPVS has
	since   time.Time          // leadership began: beats before it are unknown
}

func (s *session) dp() Dataplane { return s.n.plane }

// serve runs the data plane while this director leads: it holds the service
// VIPs, programs IPVS, checks the backends, and writes their health.
func (s *session) serve(ctx context.Context) {
	p := &plane{s: s, probers: map[string]*prober{}, weights: map[string]int{}, since: time.Now()}
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
		if err := p.step(ctx); err != nil {
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
	for _, v := range services {
		vip, err := netip.ParseAddr(v.VIP)
		if err != nil {
			continue
		}
		pool := pools[v.Pool]
		first, _ := netip.ParseAddr(pool.First)
		if !seenVIP[vip] {
			seenVIP[vip] = true
			ifc, err := s.n.local(first, pool.Interface)
			if err != nil {
				s.log.Warn("this director cannot hold a pool's VIPs", "pool", pool.Name, "error", err)
				continue
			}
			vips = append(vips, PlanVIP{Addr: vip, Interface: ifc.Interface})
		}
		proto, _ := ipvs.ProtoNumber(v.Proto)
		plan.Services = append(plan.Services, PlanService{Service: ipvs.Service{Addr: vip, Protocol: proto, Port: uint16(v.Port), Scheduler: v.Scheduler}})
		byKey[v.Name] = len(plan.Services) - 1
	}

	wantProbers := map[string]bool{}
	pinned := map[netip.Addr]bool{}
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
		wantProbers[key] = true
		spec := "tcp"
		for _, o := range offers[a.Node] {
			if o.Proto == a.Proto && o.Port == a.Port {
				spec = o.Check
			}
		}
		pr := p.prober(ctx, key, spec, netip.AddrPortFrom(addr, uint16(a.Port)), a.Healthy)
		ready := p.ready(a, n, ps.Addr, checkIns, beats, pr)
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
					s.log.Warn("backend not serving: weight 0", "service", a.Service, "node", a.Node, "at", netip.AddrPortFrom(addr, uint16(a.Port)), "reason", p.why(a, n, ps.Addr, checkIns, beats, pr))
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
	for _, v := range vips {
		if serving[v.Addr] {
			plan.VIPs = append(plan.VIPs, v)
		}
	}
	p.mu.Lock()
	for key, pr := range p.probers {
		if !wantProbers[key] {
			pr.stop()
			delete(p.probers, key)
			delete(p.weights, key)
		}
	}
	p.mu.Unlock()
	return s.dp().Apply(plan)
}

// prober is the leader's check of an assignment, started on first need; a new
// leader starts it as healthy as the state says the assignment was.
func (p *plane) prober(ctx context.Context, key, spec string, target netip.AddrPort, healthy bool) *prober {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr, ok := p.probers[key]; ok && pr.spec == spec && pr.target == target {
		return pr
	} else if ok {
		pr.stop()
	}
	cctx, stop := context.WithCancel(ctx)
	pr := &prober{spec: spec, target: target, stop: stop, state: check.State{Healthy: healthy}}
	p.probers[key] = pr
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

// ready is whether an assignment serves: the backend's heartbeat lease is
// current, its own check passes, it holds the VIP, and the leader's check
// passes. Until a new leader hears from a backend, it goes by the state.
func (p *plane) ready(a cluster.Assignment, n cluster.Node, vip netip.Addr, checkIns map[string]checkIn, beats map[string]backendLive, pr *prober) bool {
	return p.why(a, n, vip, checkIns, beats, pr) == ""
}

func (p *plane) why(a cluster.Assignment, n cluster.Node, vip netip.Addr, checkIns map[string]checkIn, beats map[string]backendLive, pr *prober) string {
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
		if !slices.Contains(b.holding, vip.String()) {
			return "it does not hold the VIP yet"
		}
	} else if !a.Healthy {
		return "no heartbeat since this director began to lead"
	}
	if !pr.healthy() {
		return "the director's check fails"
	}
	return ""
}

// liveNow is what only the leader knows, for status.
func (s *session) liveNow() ([]api.CheckIn, []api.Live, []api.Own) {
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
	stats := s.dp().Stats()
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
		if pr := p.probers[key]; pr != nil {
			pr.mu.Lock()
			if pr.state.Last != nil || pr.state.Healthy {
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
		s.backends[name] = backendLive{at: now, checks: b.Checks, holding: b.Holding, leaving: b.Leaving}
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

// known is what the cluster's state says a holder of the VIPs programs - its
// services and pinned backends, whatever their health - for Leftovers.
func (s *session) known() Plan {
	var plan Plan
	s.fsm.Read(func(st *cluster.State) {
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
