package director

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/ca"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/raftlog"
	"github.com/dmartsapp/thelb/internal/wire"
	"github.com/hashicorp/raft"
)

// session is one membership of one cluster: a Raft member identity, new at
// every start, with its key and certificate, in memory only.
type session struct {
	n      *node
	log    *logx.Logger
	raftID string
	addr   string // this member's Raft address: the node's address and the control port
	key    *ecdsa.PrivateKey
	id     *wire.Identity
	fsm    *cluster.FSM
	r      *raft.Raft
	store  *raft.InmemStore // Raft's log: a takeover replays its tail
	trans  *raft.NetworkTransport
	stream *streamLayer
	nodeLn net.Listener
	mux    *wire.Mux

	// creating is the cluster this director creates, applied when it first
	// leads; nil for a director that joined.
	creating *cluster.Create

	ctx    context.Context
	cancel context.CancelFunc
	endc   chan error
	end    sync.Once
	wg     sync.WaitGroup

	mu        sync.Mutex
	leadCtx   context.Context
	leadStop  context.CancelFunc
	vipSince  time.Time
	served    chan struct{}          // closed when this leadership's data plane is cleared
	plane     *plane                 // the leader's data plane
	backends  map[string]backendLive // the leader's: each backend's last heartbeat
	checkIns  map[string]checkIn     // the leader's: by node name
	asked     map[string]time.Time   // the leader's: when each pending node last asked
	promoting map[string]bool
	conns     map[net.Conn]struct{}
	authority *ca.Authority
	closing   bool
	member    bool       // the replicated state has this director as a member
	evict     string     // after a takeover: the director left behind, to remove once leading
	left      *peer      // after a takeover: the director left behind, which the guard knows by its MAC
	claimed   bool       // after a takeover: the control VIP is held already, found no more when it was claimed
	syncErr   string     // the IPVS sync daemons' last error, said once
	syncMu    sync.Mutex // one change to the sync daemons at a time
}

// checkIn is a member's last check-in with the leader: a director's, or a
// backend's heartbeat.
type checkIn api.CheckIn

// newSession starts a Raft member: it listens on the node's address, and - if
// it creates the cluster - bootstraps a configuration of itself alone.
func newSession(n *node, raftID string, key *ecdsa.PrivateKey, certDER []byte, caCert *x509.Certificate, creating *cluster.Create, standing *takeover) (*session, error) {
	id, err := wire.NewIdentity(caCert)
	if err != nil {
		return nil, err
	}
	if err := id.SetCertificate(key, certDER); err != nil {
		return nil, err
	}
	s := &session{
		n: n, log: n.log, raftID: raftID, key: key, id: id, fsm: cluster.New(), creating: creating,
		endc: make(chan error, 1), checkIns: map[string]checkIn{}, asked: map[string]time.Time{}, backends: map[string]backendLive{},
		promoting: map[string]bool{}, conns: map[net.Conn]struct{}{},
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.fsm.OnFatal = func(err error) { s.finish(fmt.Errorf("the cluster sent %w: upgrade this director", err)) }
	id.Revoked = func(serial string) bool {
		revoked := false
		s.fsm.Read(func(st *cluster.State) { _, revoked = st.Revoked[serial] })
		return revoked
	}

	ln, err := n.net.ListenNode()
	if err != nil {
		return nil, err
	}
	s.nodeLn, s.addr = ln, ln.Addr().String()
	if creating != nil {
		creating.Node.RaftID, creating.Node.RaftAddr = raftID, s.addr
	}
	s.stream = &streamLayer{addr: ln.Addr(), conns: make(chan net.Conn), done: make(chan struct{}), dial: s.dialRaft}
	s.mux = &wire.Mux{Handlers: map[string]func(*tls.Conn){
		wire.ALPNRaft: s.stream.push,
		wire.ALPNAPI:  s.track(s.serveAPI),
		wire.ALPNJoin: s.track(s.serveJoin),
	}}
	go s.mux.Serve(ln, id.ServerConfig([]string{"director"}, wire.ALPNRaft, wire.ALPNAPI))

	s.trans = raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream:  s.stream,
		MaxPool: 3,
		Timeout: 10 * time.Second,
		Logger:  raftlog.New(n.d.Log.Module("raft")),
	})
	conf := raft.DefaultConfig()
	conf.LocalID = raft.ServerID(raftID)
	conf.Logger = raftlog.New(n.d.Log.Module("raft"))
	conf.NoLegacyTelemetry = true
	conf.SnapshotThreshold = 1024
	conf.TrailingLogs = 1024
	conf.SnapshotInterval = 30 * time.Second
	conf.ShutdownOnRemove = true
	// In memory only, by design: every start is a new member identity, so a
	// vote can never be cast twice in a term by a voter that forgot it.
	store := raft.NewInmemStore()
	snaps := raft.NewInmemSnapshotStore()
	if standing != nil {
		if err := seed(store, snaps, s.trans, standing, s.addr); err != nil {
			_ = s.trans.Close()
			_ = s.stream.Close()
			_ = ln.Close()
			s.cancel()
			return nil, err
		}
		left := standing.left
		s.evict, s.left, s.claimed = left.name, &left, standing.claimed
	}
	s.store = store
	r, err := raft.NewRaft(conf, s.fsm, store, store, snaps, s.trans)
	if err != nil {
		_ = s.trans.Close()
		_ = s.stream.Close()
		_ = ln.Close()
		s.cancel()
		return nil, fmt.Errorf("starting Raft: %w", err)
	}
	s.r = r
	if creating != nil {
		cfg := raft.Configuration{Servers: []raft.Server{{Suffrage: raft.Voter, ID: raft.ServerID(raftID), Address: raft.ServerAddress(s.addr)}}}
		if err := r.BootstrapCluster(cfg).Error(); err != nil {
			s.close(false)
			return nil, fmt.Errorf("bootstrapping Raft: %w", err)
		}
	}
	for _, f := range []func(){s.watchLeadership, s.checkInLoop, s.monitor, s.waitMember, s.renewLoop} {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			f()
		}()
	}
	return s, nil
}

// track registers a handler's connection, so that closing the session ends it.
func (s *session) track(h func(*tls.Conn)) func(*tls.Conn) {
	return func(c *tls.Conn) {
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
			_ = c.Close()
		}()
		h(c)
	}
}

// finish ends the session with err.
func (s *session) finish(err error) {
	s.end.Do(func() { s.endc <- err })
}

// run waits until ctx is done or the session ends.
func (s *session) run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-s.endc:
		return err
	}
}

// close ends the session. On a planned stop a leader first hands leadership to
// another voter, so a restart is a short, clean failover; and every director
// then takes its member identity out of Raft, so the others keep their majority
// without it - two directors survive either one's planned stop.
func (s *session) close(planned bool) {
	if planned && s.r != nil && s.r.State() == raft.Leader && s.voters() > 1 {
		if err := s.r.LeadershipTransfer().Error(); err != nil {
			s.log.Warn("handing leadership over failed", "error", err)
		} else {
			var to raft.ServerID
			for deadline := time.Now().Add(2 * time.Second); to == "" && time.Now().Before(deadline); {
				if _, to = s.r.LeaderWithID(); to == "" {
					time.Sleep(20 * time.Millisecond)
				}
			}
			s.log.OK("leadership handed over", "to", s.nameOf(string(to)))
		}
	}
	if planned && s.r != nil && s.inRaft(s.raftID) && s.voters() > 1 {
		s.leaveRaft()
	}
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait() // the loops - and a lead() under way, which must not take the VIP after this
	s.unlead("stopping")
	s.syncDaemons(true)
	if s.r != nil {
		_ = s.r.Shutdown().Error()
	}
	if s.trans != nil {
		_ = s.trans.Close()
	}
	_ = s.stream.Close()
	_ = s.nodeLn.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.mux.Wait()
}

// inRaft is whether a member identity is in Raft's configuration.
func (s *session) inRaft(id string) bool {
	f := s.r.GetConfiguration()
	if f.Error() != nil {
		return false
	}
	for _, srv := range f.Configuration().Servers {
		if string(srv.ID) == id {
			return true
		}
	}
	return false
}

// leaveRaft takes this stopping director's member identity out of Raft: the
// leader is asked - for a few seconds, as one just handed leadership settles -
// or, if this director still leads, it removes itself. If neither works the
// others count it as a voter until it starts again, as after a crash.
func (s *session) leaveRaft() {
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if s.r.State() == raft.Leader {
			if err = s.r.RemoveServer(raft.ServerID(s.raftID), 0, 5*time.Second).Error(); err == nil {
				break
			}
			continue
		}
		if _, err = s.callLeader(api.Request{Op: api.OpLeave, RaftID: s.raftID}); err == nil {
			break
		}
	}
	if err != nil {
		s.log.Warn("leaving the vote failed: the others count this director as a voter until it starts again", "error", err)
		return
	}
	s.log.OK("out of the vote: the other directors keep their majority while this one is stopped")
}

func (s *session) voters() int {
	f := s.r.GetConfiguration()
	if f.Error() != nil {
		return 0
	}
	v := 0
	for _, srv := range f.Configuration().Servers {
		if srv.Suffrage == raft.Voter {
			v++
		}
	}
	return v
}

// nameOf is the node name of a member identity.
func (s *session) nameOf(raftID string) string {
	name := ""
	s.fsm.Read(func(st *cluster.State) {
		if n := st.NodeByRaftID(raftID); n != nil {
			name = n.Name
		}
	})
	if name == "" {
		return raftID
	}
	return name
}

// apply writes an entry through Raft, on the leader.
func (s *session) apply(typ, by string, data any) error {
	b, err := cluster.NewEntry(typ, by, s.n.d.Now(), data)
	if err != nil {
		return err
	}
	f := s.r.Apply(b, 5*time.Second)
	if err := f.Error(); err != nil {
		return err
	}
	if err, ok := f.Response().(error); ok && err != nil {
		return err
	}
	return nil
}

// leading is the context of this director's leadership, or nil when it does
// not lead.
func (s *session) leading() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leadCtx
}

// watchLeadership acts on gaining and losing leadership.
func (s *session) watchLeadership() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case isLeader := <-s.r.LeaderCh():
			if isLeader {
				s.lead()
			} else {
				s.unlead("leadership lost")
			}
		}
	}
}

// lead takes up leadership: the state brought up to date, the cluster created
// if this director creates it, then the control VIP.
func (s *session) lead() {
	// Leadership lost and won again before the loss was seen: start afresh.
	s.unlead("leadership changed")
	if err := s.r.Barrier(5 * time.Second).Error(); err != nil {
		s.log.Warn("leadership won, but the state could not be brought up to date", "error", err)
		return
	}
	created := false
	if s.creating != nil {
		exists := false
		s.fsm.Read(func(st *cluster.State) { exists = st.Cluster != nil })
		if !exists {
			if err := s.apply(cluster.TypeCreate, "bootstrap", s.creating); err != nil {
				s.finish(fmt.Errorf("creating the cluster: %w", err))
				return
			}
			created = true
		}
	}
	term := s.r.CurrentTerm()
	s.n.net.SetLease(s.vipLease())
	s.mu.Lock()
	claimed := s.claimed
	s.claimed = false
	s.mu.Unlock()
	if !claimed && !s.waitForVIP() {
		return
	}
	ln, err := s.n.net.Take()
	if err != nil {
		s.log.Error("taking the control VIP failed", "vip", s.n.cfg.ControlVIP, "error", err)
		if s.voters() > 1 {
			_ = s.r.LeadershipTransfer().Error()
		}
		return
	}
	if s.ctx.Err() != nil { // the session is closing
		s.n.net.Release()
		return
	}
	ctx, stop := context.WithCancel(s.ctx)
	now := time.Now()
	// Every member counts as live for a moment: a new leader has had no
	// check-ins yet.
	fresh := map[string]checkIn{}
	s.fsm.Read(func(st *cluster.State) {
		for name, n := range st.Nodes {
			if n.RaftID != "" && n.RaftID != s.raftID {
				fresh[name] = checkIn{Name: name, RaftID: n.RaftID, At: now, Version: n.Version}
			}
		}
	})
	served := make(chan struct{})
	s.mu.Lock()
	s.leadCtx, s.leadStop, s.vipSince, s.served = ctx, stop, now, served
	s.checkIns = fresh
	s.backends = map[string]backendLive{}
	s.mu.Unlock()
	go s.mux.Serve(ln, s.id.ServerConfig([]string{"director", "backend"}, wire.ALPNJoin, wire.ALPNAPI))

	if created {
		c := s.creating
		s.log.OK("cluster created", "cluster", c.Info.Name, "id", c.Info.ID)
		s.log.OK("certificate authority created", "fingerprint", ca.Fingerprint(s.id.CA.Raw), "not_after", s.id.CA.NotAfter)
		s.log.OK("certificate issued", "node", c.Cert.Name, "role", "director", "fingerprint", c.Cert.Fingerprint, "not_after", c.Cert.NotAfter)
	}
	if err := s.apply(cluster.TypeLeader, s.n.cfg.Name, cluster.Leader{Name: s.n.cfg.Name, RaftID: s.raftID, Term: term, Since: now.UTC(), Interface: s.n.addr.Interface}); err != nil {
		s.log.Warn("recording the leadership failed", "error", err)
	}
	s.log.OK("leader", "node", s.n.cfg.Name, "term", term, "member", s.raftID)
	s.log.OK("control VIP taken", "vip", s.n.cfg.ControlVIP, "port", s.n.cfg.ControlPort, "interface", s.n.addr.Interface)
	s.mu.Lock()
	evict := s.evict
	s.evict = ""
	s.mu.Unlock()
	if evict != "" {
		s.evictLeftBehind(evict)
	}
	s.wg.Add(3)
	go func() {
		defer s.wg.Done()
		s.sweep(ctx)
	}()
	go func() {
		defer s.wg.Done()
		s.guard(ctx)
	}()
	go func() {
		defer s.wg.Done()
		defer close(served)
		s.serve(ctx)
	}()
}

// unlead gives the control VIP up - at once: holding it needs leadership.
func (s *session) unlead(why string) {
	s.mu.Lock()
	stop, served := s.leadStop, s.served
	s.leadCtx, s.leadStop, s.served = nil, nil, nil
	s.mu.Unlock()
	if stop == nil {
		return
	}
	stop()
	if served != nil {
		<-served // the service VIPs and IPVS cleared first
	}
	s.n.net.Release()
	s.log.OK("control VIP released", "vip", s.n.cfg.ControlVIP, "reason", why)
}

// sweep tidies the membership while this director leads: member identities no
// node holds any more (a restarted director's old one), and pending nodes that
// stopped asking.
func (s *session) sweep(ctx context.Context) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		f := s.r.GetConfiguration()
		if f.Error() != nil {
			continue
		}
		current := map[string]bool{}
		var pending []string
		s.fsm.Read(func(st *cluster.State) {
			for _, n := range st.Nodes {
				if n.RaftID != "" {
					current[n.RaftID] = true
				}
			}
			pending = cluster.Sorted(st.Pending)
		})
		for _, srv := range f.Configuration().Servers {
			if id := string(srv.ID); !current[id] && id != s.raftID {
				if err := s.r.RemoveServer(srv.ID, 0, 5*time.Second).Error(); err == nil {
					s.log.OK("old member identity removed", "member", id)
				}
			}
		}
		now := time.Now()
		for _, name := range pending {
			s.mu.Lock()
			last, ok := s.asked[name]
			if !ok {
				s.asked[name] = now
			}
			s.mu.Unlock()
			if ok && now.Sub(last) > pendingFor {
				if err := s.apply(cluster.TypeDrop, s.n.cfg.Name, cluster.Name{Name: name}); err == nil {
					s.log.OK("pending node dropped: it stopped asking", "node", name)
				}
			}
		}
	}
}

// monitor ends the session when Raft has shut down by itself - this member was
// removed - and when the director has had no leader for a while and another
// cluster answers on the control VIP: the cluster was created again without it.
// One of two directors stands alone when the other is gone.
func (s *session) monitor() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var noLeaderSince, lookAt time.Time
	var notAlone string
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if s.r.State() == raft.Shutdown {
			s.finish(s.shutDown())
			return
		}
		if _, id := s.r.LeaderWithID(); id != "" {
			noLeaderSince, notAlone = time.Time{}, ""
			continue
		}
		if noLeaderSince.IsZero() {
			noLeaderSince = time.Now()
			lookAt = noLeaderSince.Add(orphanAfter)
		}
		// One of two directors, the other gone: stand alone.
		if wait, two := s.takeoverWait(); two && time.Since(noLeaderSince) >= wait {
			tk, why := s.tryTakeover(time.Since(noLeaderSince))
			if tk != nil && !s.hasLeader() { // looking takes seconds: a leader may have come
				s.finish(tk)
				return
			}
			if tk == nil && why != notAlone && !s.hasLeader() {
				notAlone = why
				s.log.Warn("no leader, and the other director may be alive: not standing alone", "reason", why)
			}
		}
		if time.Now().Before(lookAt) {
			continue
		}
		lookAt = time.Now().Add(orphanAfter) // look again after another while
		desc, same, ok := s.vipAnswer()
		if !ok || s.hasLeader() {
			// Nothing answers - or a leader came while this director looked, and
			// what answered may be itself, leading.
			continue
		}
		switch {
		case !same:
			s.finish(fmt.Errorf("%w: another cluster (%s) holds the control VIP, and this one has no leader", errRejoin, desc))
			return
		default:
			// This cluster leads on the control VIP, and not with this director:
			// it was left behind - by a takeover, or a partition.
			s.finish(fmt.Errorf("%w: this cluster leads on the control VIP without this director", errRejoin))
			return
		}
	}
}

// shutDown is why Raft shut down by itself: this member was taken out of it.
// A director the cluster still has as a member - the leader found it gone,
// and took it out of the vote only - starts over, and rejoins.
func (s *session) shutDown() error {
	approved := false
	s.fsm.Read(func(st *cluster.State) {
		n := st.Nodes[s.n.cfg.Name]
		approved = n != nil && n.Role == "director" && n.RaftID == s.raftID
	})
	if approved {
		return fmt.Errorf("%w: this director was taken out of the vote", errRejoin)
	}
	return errRemoved
}

// waitForVIP waits, as a new leader, until the control VIP is no more - two
// looks, a second apart (vipGone) - and reports whether it may take it: false
// once it no longer leads, or the session ends. A director never claims an
// address another machine still answers for: a theLB killed leaves its VIP on
// its interface until the lease lapses; one answering after that is held by
// hand, or by a theLB alive, and the leader goes on waiting, saying so.
func (s *session) waitForVIP() bool {
	start := time.Now()
	said := ""
	for {
		if s.ctx.Err() != nil || s.r.State() != raft.Leader {
			return false
		}
		gone, holder, err := s.vipGone()
		if gone {
			if said != "" {
				s.log.OK("the control VIP is no more: taking it", "vip", s.n.cfg.ControlVIP)
			}
			return true
		}
		if s.ctx.Err() != nil {
			return false
		}
		lease := s.vipLease()
		why := fmt.Sprintf("%s still answers for it: this leader takes it once it is no more - within the %s lease, if a theLB that stopped left it", holder, lease)
		switch {
		case err != nil:
			why = err.Error()
		case time.Since(start) > lease+leaseSlack:
			why = fmt.Sprintf("%s still answers for it, past the %s lease: set there by hand, or by a theLB alive - remove it there (ip address del)", holder, lease)
		}
		if why != said {
			said = why
			s.log.Warn("another machine answers for the control VIP: this leader waits", "vip", s.n.cfg.ControlVIP, "reason", why)
		}
		select {
		case <-s.ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}

// hasLeader is whether Raft knows a leader - this director or another.
func (s *session) hasLeader() bool {
	_, id := s.r.LeaderWithID()
	return id != "" || s.leading() != nil
}

// vipAnswer is which cluster answers theLB's handshake on the control VIP: none
// (ok false), this one (same), or another, described by its name and CA.
func (s *session) vipAnswer() (desc string, same, ok bool) {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	c, err := s.n.net.DialVIP(ctx)
	if err != nil {
		return "", false, false
	}
	var cl wire.Cluster
	tc := tls.Client(c, wire.JoinConfig(s.n.cfg.ControlVIP, &cl))
	_ = tc.SetDeadline(time.Now().Add(2 * time.Second))
	err = tc.Handshake()
	_ = tc.Close()
	if err != nil || cl.CA == nil {
		return "", false, false
	}
	if cl.CA.Equal(s.id.CA) {
		return cl.Name, true, true
	}
	return cl.Name + ", CA " + ca.Fingerprint(cl.CA.Raw)[:16], false, true
}

// waitMember reports the director a member once its state has caught up with
// its own admission - and then watches that it stays one: Raft does not always
// tell a removed member, but the replicated state does.
func (s *session) waitMember() {
	member := false
	for {
		ch := s.fsm.Changed()
		var n *cluster.Node
		created := false
		s.fsm.Read(func(st *cluster.State) {
			created = st.Cluster != nil
			if p := st.Nodes[s.n.cfg.Name]; p != nil {
				c := *p
				n = &c
			}
		})
		switch {
		case !member && created && n != nil && n.RaftID == s.raftID:
			member = true
			s.mu.Lock()
			s.member = true
			s.mu.Unlock()
			s.n.member(s)
			if s.leading() == nil {
				if err := s.dp().Leftovers(s.known()); err != nil {
					s.log.Warn("removing what an earlier run left in the kernel failed", "error", err)
				}
			}
			s.syncDaemons(false) // the backup: the leader's connections, taken in
		case member && n == nil:
			s.finish(errRemoved)
			return
		case member && n.RaftID != s.raftID:
			s.finish(fmt.Errorf("%w: another member identity, %s, holds this director's name", errRejoin, n.RaftID))
			return
		}
		select {
		case <-s.ctx.Done():
			return
		case <-ch:
		}
	}
}

// renewLoop renews this director's certificate two thirds of the way through
// it: the leader signs its own, the others ask the leader.
func (s *session) renewLoop() {
	for {
		leaf := s.id.Leaf()
		wait := renewIn(leaf, time.Now())
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := s.renew(); err != nil {
			s.log.Error("renewing the director's certificate failed; retrying in a minute", "error", err)
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}
}

func (s *session) renew() error {
	csr, err := ca.Request(s.key, s.n.cfg.Name)
	if err != nil {
		return err
	}
	var der []byte
	if s.leading() != nil {
		der, err = s.sign(s.n.cfg.Name, csr)
	} else {
		var resp api.Response
		resp, err = s.callLeader(api.Request{Op: api.OpRenew, CSR: csr})
		der = resp.Cert
	}
	if err != nil {
		return err
	}
	if err := s.id.SetCertificate(s.key, der); err != nil {
		return err
	}
	leaf := s.id.Leaf()
	s.log.OK("certificate renewed", "node", s.n.cfg.Name, "fingerprint", ca.Fingerprint(leaf.Raw), "not_after", leaf.NotAfter)
	return nil
}

// renewIn is when to renew a certificate: two thirds of the way through it.
func renewIn(c *x509.Certificate, now time.Time) time.Duration {
	life := c.NotAfter.Sub(c.NotBefore)
	at := c.NotBefore.Add(life * 2 / 3)
	return max(at.Sub(now), time.Second)
}

// signer is the cluster's CA, from the replicated state.
func (s *session) signer() (*ca.Authority, error) {
	s.mu.Lock()
	a := s.authority
	s.mu.Unlock()
	if a != nil {
		return a, nil
	}
	var certDER, keyDER []byte
	s.fsm.Read(func(st *cluster.State) {
		if st.Cluster != nil {
			certDER, keyDER = st.Cluster.CACert, st.Cluster.CAKey
		}
	})
	if certDER == nil {
		return nil, errors.New("the cluster's state has no CA yet")
	}
	a, err := ca.Load(certDER, keyDER)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.authority = a
	s.mu.Unlock()
	return a, nil
}

// sign signs, on the leader, a renewed certificate for the approved node name
// - for the key in its request, with its approved address, and a director's
// the control VIP too - and records it.
func (s *session) sign(name string, csrDER []byte) ([]byte, error) {
	csr, err := ca.ParseRequest(csrDER)
	if err != nil {
		return nil, err
	}
	var addr, role string
	s.fsm.Read(func(st *cluster.State) {
		if n := st.Nodes[name]; n != nil {
			addr, role = n.Address, n.Role
		}
	})
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return nil, fmt.Errorf("%s is not a node of this cluster", name)
	}
	ips := []netip.Addr{ip, s.n.cfg.ControlVIP}
	if role == "backend" {
		ips = []netip.Addr{ip}
	}
	a, err := s.signer()
	if err != nil {
		return nil, err
	}
	cert, der, err := a.Sign(role, name, ips, csr.PublicKey, s.n.d.Now(), ca.NodeLifetime)
	if err != nil {
		return nil, err
	}
	if err := s.apply(cluster.TypeCert, s.n.cfg.Name, certRecord(name, role, cert)); err != nil {
		return nil, err
	}
	return der, nil
}

func certRecord(name, kind string, c *x509.Certificate) cluster.Cert {
	return cluster.Cert{Name: name, Kind: kind, Serial: ca.Serial(c), Fingerprint: ca.Fingerprint(c.Raw), NotAfter: c.NotAfter.UTC()}
}

// dialRaft connects to another director's Raft, with mutual TLS.
func (s *session) dialRaft(address string, timeout time.Duration) (net.Conn, error) {
	return s.dialMember(address, wire.ALPNRaft, timeout)
}

func (s *session) dialMember(address, proto string, timeout time.Duration) (*tls.Conn, error) {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return nil, fmt.Errorf("a member's address %q: %w", address, err)
	}
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()
	c, err := s.n.net.Dial(ctx, address)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(c, s.id.ClientConfig(proto, ap.Addr()))
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return tc, nil
}

// streamLayer is Raft's transport over the control port: connections arrive
// through the mux, already past mutual TLS.
type streamLayer struct {
	addr  net.Addr
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
	dial  func(address string, timeout time.Duration) (net.Conn, error)
}

func (l *streamLayer) push(c *tls.Conn) {
	select {
	case l.conns <- c:
	case <-l.done:
		_ = c.Close()
	}
}

func (l *streamLayer) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *streamLayer) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *streamLayer) Addr() net.Addr { return l.addr }

func (l *streamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	return l.dial(string(address), timeout)
}
