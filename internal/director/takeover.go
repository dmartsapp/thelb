package director

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/raft"

	"github.com/dmartsapp/thelb/internal/cluster"
)

// A cluster of two directors loses its majority when either one crashes: the
// crashed one's member identity stays in Raft, counting, and never votes again
// (Raft is in memory). So the one left, once it has gone without a leader for
// the control VIP's lease - any VIP the other held has lapsed by then, unless
// it is alive - and finds the other gone, not merely out of reach, and the VIP
// no more, stands alone: it starts a new session of itself alone, from the
// state it had, and removes the other, which waits for approval when it comes
// back. It never takes a VIP another machine still answers for.
const (
	// takeoverAfter: longer than an election takes, shorter than the 5 s a
	// restarted director looks at the control VIP before creating a cluster.
	takeoverAfter = 3 * time.Second
	// takeoverStagger delays the director whose member identity sorts later. If
	// both are alive - a firewall rejecting the control port between them, say -
	// and each finds the other gone, the first claims the control VIP as soon as
	// it decides, and the other, looking later, finds it held, and stays. It
	// covers the head start a leader that stepped down has, in noticing there
	// is no leader, over a follower (up to two seconds), with room to spare: both
	// look at the VIP last, the same way.
	takeoverStagger = 6 * time.Second
	// leaseSlack is added to the lease before a VIP still answering is taken
	// for more than a lease not yet lapsed: the kernel removes an address up to
	// a second late.
	leaseSlack = 2 * time.Second
)

// peer is another director, as a takeover leaves it behind and the leader's
// guard finds it.
type peer struct {
	name, mac string
	raftID    string // its member identity; "" when not known
	addr      string // its Raft address: its node's address and the control port
}

// takeover ends a session whose director stands alone: what the node needs to
// start the next one.
type takeover struct {
	raftID      string
	key         *ecdsa.PrivateKey
	certDER     []byte
	caCert      *x509.Certificate
	state       []byte // the cluster's state, as Raft snapshots it
	index, term uint64
	replayed    int           // entries past what this director had applied, replayed into state
	left        peer          // the director left behind
	lease       time.Duration // the control VIP's lease
	claimed     bool          // the control VIP was claimed for the next session
	why         string
}

func (t *takeover) Error() string { return "standing alone: " + t.why }

// takeoverFacts is what a director sees when it considers standing alone.
type takeoverFacts struct {
	other, otherMAC string        // the other voter: its name and MAC address
	waited          time.Duration // without a leader so far
	lease           time.Duration // the control VIP's lease, from the replicated state
	vipHolder       string        // who still answers for the control VIP, after two looks: a MAC address, or ""
	uplink          bool          // this director still reaches its network
	uplinkWhy       string        // why not
	portErr         error         // connecting to the other's control port; nil: it accepted
	answers         string        // who answers for the other's address, when its port did not refuse
}

// standAlone decides whether this director stands alone: the lease waited out,
// the other gone - its theLB not running, or its machine not answering - its
// own network reached, and the control VIP no more. If not, it says why not.
// A VIP still answering is never taken: a theLB killed leaves its VIP on the
// interface only until the lease lapses, so one answering after that is held
// by a theLB alive - behind a firewall - or by hand.
func standAlone(f takeoverFacts) (bool, string) {
	switch {
	case f.waited < f.lease+leaseSlack:
		return false, fmt.Sprintf("waiting out the control VIP's %s lease: a theLB that stopped may still hold it", f.lease)
	case !f.uplink:
		return false, "this director does not reach its network: " + f.uplinkWhy
	}
	if gone, why := isGone(f.other, f.portErr, f.answers); !gone {
		return false, why
	}
	switch {
	case f.vipHolder == tcpServer:
		return false, "something accepts connections on the control VIP"
	case f.vipHolder != "" && !strings.EqualFold(f.vipHolder, f.otherMAC):
		return false, f.vipHolder + " holds the control VIP"
	case f.vipHolder != "":
		return false, fmt.Sprintf("%s's machine still answers for the control VIP, past its %s lease: its theLB runs, behind a firewall, or the address was set there by hand - remove it there (ip address del)", f.other, f.lease)
	}
	_, why := isGone(f.other, f.portErr, f.answers)
	return true, why
}

// isGone decides, from connecting to a director's control port and asking the
// link for its address, whether it is gone - its theLB not running, or its
// machine not answering - and, if not, says why not. One that answers on the
// network while its port drops connections is behind a partition, perhaps a
// firewall: it may be alive, leading or about to.
func isGone(name string, portErr error, answers string) (bool, string) {
	switch {
	case portErr == nil:
		return false, name + " accepts connections on its control port"
	case errors.Is(portErr, syscall.ECONNREFUSED):
		return true, name + "'s control port refuses connections: its theLB is not running"
	case answers != "":
		return false, name + " answers on the network, but not on its control port: a partition, not a crash"
	}
	return true, name + " does not answer on the network"
}

// look connects to a director's control port and - unless it refuses - asks the
// link who has its address: what isGone decides from.
func (s *session) look(ctx context.Context, addr string) (portErr error, answers string, err error) {
	dctx, cancel := context.WithTimeout(ctx, time.Second)
	c, portErr := s.n.net.Dial(dctx, addr)
	cancel()
	if portErr == nil {
		_ = c.Close()
		return nil, "", nil
	}
	if errors.Is(portErr, syscall.ECONNREFUSED) {
		return portErr, "", nil
	}
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return portErr, "", err
	}
	answers, err = s.n.net.Holder(ctx, ap.Addr())
	return portErr, answers, err
}

// otherVoter is the other voter, when Raft's configuration has exactly two and
// this director is one.
func (s *session) otherVoter() (raft.Server, bool) {
	f := s.r.GetConfiguration()
	if f.Error() != nil {
		return raft.Server{}, false
	}
	var other raft.Server
	voters, self := 0, false
	for _, srv := range f.Configuration().Servers {
		if srv.Suffrage != raft.Voter {
			continue
		}
		voters++
		if string(srv.ID) == s.raftID {
			self = true
		} else {
			other = srv
		}
	}
	return other, voters == 2 && self
}

// takeoverWait is how long this director goes without a leader before it
// considers standing alone - the control VIP's lease, waited out - and whether
// it may at all: one of exactly two voters.
func (s *session) takeoverWait() (time.Duration, bool) {
	other, ok := s.otherVoter()
	if !ok {
		return 0, false
	}
	wait := max(takeoverAfter, s.vipLease()+leaseSlack)
	if s.raftID > string(other.ID) {
		wait += takeoverStagger
	}
	return wait, true
}

// vipLease is the control VIP's lease, from the replicated state.
func (s *session) vipLease() time.Duration {
	var d time.Duration
	s.fsm.Read(func(st *cluster.State) { d = st.VIPLease() })
	return d
}

// vipGone is whether the control VIP is no more: two looks, a second apart,
// each finding no machine answering ARP or neighbour solicitation for it and
// nothing accepting a connection on its port. Otherwise it returns who holds
// it - a MAC address, or tcpServer when only the port answered.
func (s *session) vipGone() (bool, string, error) {
	for i := range 2 {
		if i > 0 {
			select {
			case <-s.ctx.Done():
				return false, "", s.ctx.Err()
			case <-time.After(time.Second):
			}
		}
		holder, err := s.lookAtVIP()
		if err != nil || holder != "" {
			return false, holder, err
		}
	}
	return true, "", nil
}

// lookAtVIP asks the link who has the control VIP, and connects to its port, at
// once: one look of vipGone's.
func (s *session) lookAtVIP() (string, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 1500*time.Millisecond)
	defer cancel()
	accepted := make(chan bool, 1)
	go func() {
		c, err := s.n.net.DialVIP(ctx)
		if err == nil {
			_ = c.Close()
		}
		accepted <- err == nil
	}()
	holder, err := s.n.net.VIPHolder(ctx)
	ok := <-accepted
	switch {
	case err != nil:
		return "", fmt.Errorf("asking the link who has the control VIP: %w", err)
	case holder != "":
		return holder, nil
	case ok:
		return tcpServer, nil
	}
	return "", nil
}

// tryTakeover looks at the other voter, this director's network and the
// control VIP - the VIP last, so a director that decided first and claimed it
// is seen - and returns what the next session needs if this director is to
// stand alone, or why not. waited is how long it has had no leader.
func (s *session) tryTakeover(waited time.Duration) (*takeover, string) {
	other, ok := s.otherVoter()
	if !ok {
		return nil, "not one of two voters"
	}
	var n cluster.Node
	found := false
	s.fsm.Read(func(st *cluster.State) {
		if p := st.NodeByRaftID(string(other.ID)); p != nil {
			n, found = *p, true
		}
	})
	if !found || n.Role != "director" {
		return nil, "the other voter is no director of this cluster's state"
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	f := takeoverFacts{other: n.Name, otherMAC: n.MAC, waited: waited, lease: s.vipLease()}
	if f.waited >= f.lease+leaseSlack {
		f.uplink, f.uplinkWhy = s.n.net.Uplink(ctx)
	}
	var err error
	if f.uplink {
		if f.portErr, f.answers, err = s.look(ctx, string(other.Address)); err != nil {
			return nil, "looking for " + n.Name + " on the network failed: " + err.Error()
		}
	}
	if gone, _ := isGone(f.other, f.portErr, f.answers); gone && f.uplink {
		if _, f.vipHolder, err = s.vipGone(); err != nil {
			return nil, "looking at the control VIP failed: " + err.Error()
		}
	}
	yes, why := standAlone(f)
	if !yes {
		return nil, why
	}
	state, err := s.fsm.Bytes()
	if err != nil {
		return nil, "reading the cluster's state failed: " + err.Error()
	}
	applied := s.r.AppliedIndex()
	state, last, replayed, err := replay(state, s.store, applied)
	if err != nil {
		return nil, "replaying Raft's log failed: " + err.Error()
	}
	leaf := s.id.Leaf()
	if leaf == nil {
		return nil, "this director has no certificate"
	}
	return &takeover{
		raftID: s.raftID, key: s.key, certDER: leaf.Raw, caCert: s.id.CA,
		state: state, index: max(last, applied), term: s.r.CurrentTerm(), replayed: replayed,
		left: peer{name: n.Name, mac: n.MAC, raftID: string(other.ID), addr: string(other.Address)}, lease: f.lease, why: why,
	}, why
}

// replay brings a copy of the state to the end of this director's log, as
// hashicorp/raft's RecoverCluster does: the entries past what the state has
// applied are ones the leader had sent - and may have confirmed to whoever
// asked for the change - before it could say they were committed. applied is
// where to start when the state does not say (a snapshot from an older theLB).
// It returns the state, the last index it covers, and how many entries it
// applied.
func replay(state []byte, logs raft.LogStore, applied uint64) ([]byte, uint64, int, error) {
	f := cluster.New()
	if err := f.Restore(io.NopCloser(bytes.NewReader(state))); err != nil {
		return nil, 0, 0, err
	}
	f.Read(func(st *cluster.State) {
		if st.Applied > 0 {
			applied = st.Applied // read with the state itself: exact
		}
	})
	first, err := logs.FirstIndex()
	if err != nil {
		return nil, 0, 0, err
	}
	last, err := logs.LastIndex()
	if err != nil {
		return nil, 0, 0, err
	}
	n := 0
	for i := max(applied+1, first); i <= last; i++ {
		var l raft.Log
		if err := logs.GetLog(i, &l); err != nil {
			return nil, 0, 0, fmt.Errorf("entry %d: %w", i, err)
		}
		if l.Type != raft.LogCommand {
			continue
		}
		if err, ok := f.Apply(&l).(error); ok && errors.Is(err, cluster.ErrUnknownEntry) {
			return nil, 0, 0, err
		}
		n++
	}
	if n == 0 {
		return state, max(last, applied), 0, nil
	}
	b, err := f.Bytes()
	return b, max(last, applied), n, err
}

// standAlone starts a session of this director alone - its member identity,
// key and certificate, and the cluster's state as it had it - after the other
// of two is gone.
func (n *node) standAlone(t *takeover) (*session, error) {
	n.log.Warn("standing alone: the other director is gone; this one goes on as the cluster's only voter, and removes it", "node", t.left.name, "reason", t.why)
	n.takeovers.Add(1)
	if t.replayed > 0 {
		n.log.OK("the last entries of Raft's log replayed: changes the leader may have confirmed before it went are kept", "entries", t.replayed)
	}
	// The control VIP - just found to be no more - claimed at once, before this
	// director leads: if the other is alive after all, and deciding too, it
	// finds the VIP held, and stays.
	n.net.SetLease(t.lease)
	if _, err := n.net.Take(); err != nil {
		n.log.Warn("claiming the control VIP failed; this director takes it when it leads, once it is no more", "vip", n.cfg.ControlVIP, "error", err)
	} else {
		t.claimed = true
	}
	s, err := newSession(n, t.raftID, t.key, t.certDER, t.caCert, nil, t)
	if err != nil {
		n.net.Release()
		return nil, err
	}
	return s, nil
}

// seed gives a new Raft the state a takeover carries, as a snapshot whose
// configuration is this director alone - what hashicorp/raft's RecoverCluster
// writes - and the term it had reached, so the terms it goes on with are later.
func seed(store *raft.InmemStore, snaps *raft.InmemSnapshotStore, trans raft.Transport, t *takeover, addr string) error {
	cfg := raft.Configuration{Servers: []raft.Server{{Suffrage: raft.Voter, ID: raft.ServerID(t.raftID), Address: raft.ServerAddress(addr)}}}
	sink, err := snaps.Create(raft.SnapshotVersionMax, t.index, t.term, cfg, 1, trans)
	if err != nil {
		return fmt.Errorf("seeding Raft: %w", err)
	}
	if _, err := sink.Write(t.state); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("seeding Raft: %w", err)
	}
	if err := sink.Close(); err != nil {
		return fmt.Errorf("seeding Raft: %w", err)
	}
	return store.SetUint64([]byte("CurrentTerm"), t.term)
}

// evictLeftBehind removes the director a takeover left behind, as `admin
// remove` would: its certificate revoked, it waits for approval when it comes
// back - whether it crashed, or was alive behind a partition all along.
func (s *session) evictLeftBehind(name string) {
	if _, err := s.remove(s.n.cfg.Name, name); err != nil {
		s.log.Warn("removing the director left behind failed", "node", name, "error", err)
		return
	}
	s.log.Warn("the director left behind is removed: it waits for approval when it comes back", "node", name)
}
