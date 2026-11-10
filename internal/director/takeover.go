package director

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"errors"
	"fmt"
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
	// takeoverStagger delays the director whose member identity sorts later: if
	// both are alive and each finds the other gone, the first takes the control
	// VIP, and the other then finds it answered, and stays.
	takeoverStagger = 3 * time.Second
)

// takeover ends a session whose director stands alone: what the node needs to
// start the next one.
type takeover struct {
	raftID      string
	key         *ecdsa.PrivateKey
	certDER     []byte
	caCert      *x509.Certificate
	state       []byte // the cluster's state, as Raft snapshots it
	index, term uint64
	evict       string // the director left behind
	why         string
}

func (t *takeover) Error() string { return "standing alone: " + t.why }

// takeoverFacts is what a director sees when it considers standing alone.
type takeoverFacts struct {
	other, otherMAC string // the other voter: its name and MAC address
	vipHolder       string // who answers ARP for the control VIP: a MAC address, or ""
	vipTheLB        bool   // theLB's handshake is answered on the control VIP
	uplink          bool   // this director still reaches its network
	uplinkWhy       string // why not
	portErr         error  // connecting to the other's control port; nil: it accepted
	answers         string // who answers for the other's address, when its port did not refuse
}

// standAlone decides whether the other of two directors is gone - its theLB not
// running, or its machine not answering - and, if not, says why not. A director
// that answers on the network while its port drops connections is behind a
// partition, perhaps a firewall: it may be leading, or about to.
func standAlone(f takeoverFacts) (bool, string) {
	switch {
	case f.vipTheLB:
		return false, "a theLB answers on the control VIP"
	case f.vipHolder != "" && !strings.EqualFold(f.vipHolder, f.otherMAC):
		return false, f.vipHolder + " holds the control VIP"
	case !f.uplink:
		return false, "this director does not reach its network: " + f.uplinkWhy
	case f.portErr == nil:
		return false, f.other + " accepts connections on its control port"
	case errors.Is(f.portErr, syscall.ECONNREFUSED):
		return true, f.other + "'s control port refuses connections: its theLB is not running"
	case f.answers != "":
		return false, f.other + " answers on the network, but not on its control port: a partition, not a crash"
	}
	return true, f.other + " does not answer on the network"
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

// tryTakeover looks at the other voter and the control VIP, and returns what
// the next session needs if this director is to stand alone - or why not.
func (s *session) tryTakeover() (*takeover, string) {
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
	f := takeoverFacts{other: n.Name, otherMAC: n.MAC}
	var err error
	if f.vipHolder, err = s.n.net.VIPHolder(ctx); err != nil {
		return nil, "looking at the control VIP failed: " + err.Error()
	}
	_, _, f.vipTheLB = s.vipAnswer()
	f.uplink, f.uplinkWhy = s.n.net.Uplink(ctx)
	dctx, dcancel := context.WithTimeout(ctx, time.Second)
	c, err := s.n.net.Dial(dctx, string(other.Address))
	dcancel()
	if err == nil {
		_ = c.Close()
	}
	f.portErr = err
	if err != nil && !errors.Is(err, syscall.ECONNREFUSED) {
		if ap, perr := netip.ParseAddrPort(string(other.Address)); perr == nil {
			if f.answers, err = s.n.net.Holder(ctx, ap.Addr()); err != nil {
				return nil, "looking for " + n.Name + " on the network failed: " + err.Error()
			}
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
	leaf := s.id.Leaf()
	if leaf == nil {
		return nil, "this director has no certificate"
	}
	return &takeover{
		raftID: s.raftID, key: s.key, certDER: leaf.Raw, caCert: s.id.CA,
		state: state, index: s.r.AppliedIndex(), term: s.r.CurrentTerm(), evict: n.Name, why: why,
	}, why
}

// standAlone starts a session of this director alone - its member identity,
// key and certificate, and the cluster's state as it had it - after the other
// of two is gone.
func (n *node) standAlone(t *takeover) (*session, error) {
	n.log.Warn("standing alone: the other director is gone; this one goes on as the cluster's only voter, and removes it", "node", t.evict, "reason", t.why)
	return newSession(n, t.raftID, t.key, t.certDER, t.caCert, nil, t)
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
