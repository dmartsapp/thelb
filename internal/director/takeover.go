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
// (Raft is in memory). So the one left, once it has gone takeoverAfter without
// a leader and finds the other gone - not merely out of reach - stands alone: it
// starts a new session of itself alone, from the state it had, and removes the
// other, which waits for approval when it comes back.
const (
	// takeoverAfter: longer than an election takes, shorter than the 5 s a
	// restarted director looks at the control VIP before creating a cluster.
	takeoverAfter = 3 * time.Second
	// takeoverStagger delays the director whose member identity sorts later. If
	// both are alive - a firewall rejecting the control port between them, say -
	// and each finds the other gone, the first claims the control VIP as soon as
	// it decides, and the other, looking later, finds the VIP it saw free taken:
	// the first is alive, and leads. It covers the first one's look (about a
	// second) and the head start a leader that stepped down has, in noticing
	// there is no leader, over a follower (up to two), with room to spare.
	takeoverStagger = 6 * time.Second
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
	replayed    int  // entries past what this director had applied, replayed into state
	left        peer // the director left behind
	why         string
}

func (t *takeover) Error() string { return "standing alone: " + t.why }

// takeoverFacts is what a director sees when it considers standing alone.
type takeoverFacts struct {
	other, otherMAC string // the other voter: its name and MAC address
	otherLed        bool   // the other was the last leader this director saw
	vipHolder       string // who answers ARP for the control VIP: a MAC address, or ""
	vipWasFree      bool   // this director saw the control VIP free since it lost its leader
	vipTheLB        bool   // theLB's handshake is answered on the control VIP
	uplink          bool   // this director still reaches its network
	uplinkWhy       string // why not
	portErr         error  // connecting to the other's control port; nil: it accepted
	answers         string // who answers for the other's address, when its port did not refuse
}

// standAlone decides whether the other of two directors is gone - its theLB not
// running, or its machine not answering - and, if not, says why not.
//
// The control VIP held by the other's machine is what a theLB killed leaves on
// its interface - but only when the other was the leader, and held it at every
// look since: a VIP this director saw free, then taken, was taken by a director
// alive, which decided first.
func standAlone(f takeoverFacts) (bool, string) {
	held := f.vipHolder != ""
	switch {
	case f.vipTheLB:
		return false, "a theLB answers on the control VIP"
	case held && !strings.EqualFold(f.vipHolder, f.otherMAC):
		return false, f.vipHolder + " holds the control VIP"
	case held && f.vipWasFree:
		return false, f.other + " took the control VIP after this director saw it free: it is alive, and leads"
	case held && !f.otherLed:
		return false, f.other + " holds the control VIP, and was not the leader this director saw last"
	case !f.uplink:
		return false, "this director does not reach its network: " + f.uplinkWhy
	}
	return isGone(f.other, f.portErr, f.answers)
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
// considers standing alone - and whether it may at all: one of exactly two
// voters.
func (s *session) takeoverWait() (time.Duration, bool) {
	other, ok := s.otherVoter()
	if !ok {
		return 0, false
	}
	if s.raftID > string(other.ID) {
		return takeoverAfter + takeoverStagger, true
	}
	return takeoverAfter, true
}

// vipFree is whether no machine answers for the control VIP: a look that fails
// says nothing.
func (s *session) vipFree() bool {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	holder, err := s.n.net.VIPHolder(ctx)
	return err == nil && holder == "" && ctx.Err() == nil
}

// tryTakeover looks at the other voter and the control VIP, and returns what
// the next session needs if this director is to stand alone - or why not.
// lastLeader is the member identity of the last leader this director saw, and
// vipWasFree whether it has seen the control VIP free since.
func (s *session) tryTakeover(lastLeader string, vipWasFree bool) (*takeover, string) {
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
	f := takeoverFacts{other: n.Name, otherMAC: n.MAC, otherLed: lastLeader == string(other.ID), vipWasFree: vipWasFree}
	var err error
	if f.vipHolder, err = s.n.net.VIPHolder(ctx); err != nil {
		return nil, "looking at the control VIP failed: " + err.Error()
	}
	if f.vipHolder != "" { // no connection reaches a VIP no machine answers for
		_, _, f.vipTheLB = s.vipAnswer()
	}
	f.uplink, f.uplinkWhy = s.n.net.Uplink(ctx)
	if f.portErr, f.answers, err = s.look(ctx, string(other.Address)); err != nil {
		return nil, "looking for " + n.Name + " on the network failed: " + err.Error()
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
		left: peer{name: n.Name, mac: n.MAC, raftID: string(other.ID), addr: string(other.Address)}, why: why,
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
	if t.replayed > 0 {
		n.log.OK("the last entries of Raft's log replayed: changes the leader may have confirmed before it went are kept", "entries", t.replayed)
	}
	// The control VIP claimed at once, before this director leads: if the other
	// is alive after all, and deciding too, it finds the VIP taken, and stays.
	if _, err := n.net.Take(); err != nil {
		n.log.Warn("claiming the control VIP failed; this director takes it when it leads", "vip", n.cfg.ControlVIP, "error", err)
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
