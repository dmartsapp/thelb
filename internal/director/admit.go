package director

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/dmartsapp/thelb/internal/ca"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/secret"
	"github.com/dmartsapp/thelb/internal/wire"
	"github.com/hashicorp/raft"
)

// What the leader makes of a node asking to join.
type verdict int

const (
	verdictPending  verdict = iota // wait for a director's approval
	verdictApproved                // approved, now or before: admit it
	verdictRefused                 // a director refused it
	verdictClone                   // another live node has its fingerprint
	verdictBusy                    // its name is held by a live member
)

type admission struct {
	verdict verdict
	by      string // who approved or refused it
	why     string
	pending bool // pending already, as it claims to be
}

// live is whether the node holding name - with member identity raftID - is
// running: the leader itself, or a director that checked in a moment ago.
func (s *session) live(checkIns map[string]checkIn, name, raftID string) bool {
	if raftID == "" {
		return false
	}
	if raftID == s.raftID {
		return true
	}
	ci, ok := checkIns[name]
	return ok && ci.RaftID == raftID && time.Since(ci.At) < liveFor
}

func (s *session) checkInsCopy() map[string]checkIn {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[string]checkIn, len(s.checkIns))
	for k, v := range s.checkIns {
		m[k] = v
	}
	return m
}

// admission decides what to do with a node's claim - the design's approval
// rules: approved once, a node rejoins by itself while the cluster remembers
// it, as long as its fingerprint, address and MAC address are the same.
func (s *session) admission(c cluster.Claim) admission {
	live := s.checkInsCopy()
	var a admission
	s.fsm.Read(func(st *cluster.State) {
		if r := st.Refused[c.Name]; r != nil && r.Same(c) {
			a = admission{verdict: verdictRefused, by: r.By, why: "refused by " + r.By}
			return
		}
		for name, n := range st.Nodes {
			if name != c.Name && n.Fingerprint == c.Fingerprint && s.live(live, name, n.RaftID) {
				a = admission{verdict: verdictClone, why: fmt.Sprintf("its node fingerprint is that of %s, which is running: two live nodes cannot share one (a cloned machine?)", name)}
				return
			}
		}
		if n := st.Nodes[c.Name]; n != nil {
			if s.live(live, c.Name, n.RaftID) {
				a = admission{verdict: verdictBusy, why: fmt.Sprintf("the name %s is held by a member that is running; trying again", c.Name)}
				return
			}
			if n.Same(c) {
				a = admission{verdict: verdictApproved, by: n.ApprovedBy}
				return
			}
		}
		p := st.Pending[c.Name]
		a = admission{verdict: verdictPending, pending: p != nil && p.Same(c)}
	})
	return a
}

func remoteIP(c net.Conn) netip.Addr {
	ap, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

// serveJoin answers a node asking to join, on the control VIP.
func (s *session) serveJoin(c *tls.Conn) {
	w := wire.NewConn(c)
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	from := remoteIP(c)
	var hello joinHello
	if err := w.Recv(&hello); err != nil {
		return // a probe: the handshake was all it wanted
	}
	lctx := s.leading()
	if lctx == nil {
		_ = w.Send(joinReply{V: wire.Version, Retry: true, Error: "the director answering does not lead at the moment"})
		return
	}
	if hello.V != wire.Version || hello.Role != "director" {
		_ = w.Send(joinReply{V: wire.Version, Error: "this cluster takes joins from directors of this protocol version only"})
		return
	}
	cs := c.ConnectionState()
	s.n.mu.Lock()
	dirSecret := s.n.dirSecret
	s.n.mu.Unlock()
	if !wire.CheckProof([]secret.Secret{dirSecret}, wire.SideJoiner, cs, hello.Proof) {
		s.log.Warn("join refused: the directors' secret was not proved", "from", from)
		_ = w.Send(joinReply{V: wire.Version, Error: "not accepted"})
		return
	}
	proof, err := wire.Proof(dirSecret, wire.SideCluster, cs)
	if err != nil {
		return
	}
	var clusterID string
	s.fsm.Read(func(st *cluster.State) {
		if st.Cluster != nil {
			clusterID = st.Cluster.ID
		}
	})
	if err := w.Send(joinReply{V: wire.Version, OK: true, Proof: proof, ClusterID: clusterID}); err != nil {
		return
	}
	var req joinRequest
	if err := w.Recv(&req); err != nil {
		return
	}
	claim := req.Claim
	refuse := func(why string) {
		s.log.Warn("join refused", "node", claim.Name, "from", from, "reason", why)
		_ = w.Send(joinState{State: stateRefused, Message: why})
	}
	if p := claimProblem(claim, from); p != "" {
		refuse(p)
		return
	}
	csr, err := ca.ParseRequest(req.CSR)
	if err != nil || csr.Subject.CommonName != claim.Name {
		refuse("its certificate signing request is not for its name")
		return
	}
	if mac := s.n.net.MACOf(from); mac != "" && !strings.EqualFold(mac, claim.MAC) {
		refuse(fmt.Sprintf("the MAC address on the wire, %s, is not the one claimed, %s", mac, claim.MAC))
		return
	}
	_ = c.SetDeadline(time.Time{})

	// The joiner's messages: "here" while it waits, "ready" when it listens.
	msgs, gone, done := make(chan joinMsg), make(chan struct{}), make(chan struct{})
	defer close(done)
	go func() {
		defer close(gone)
		for {
			_ = c.SetReadDeadline(time.Now().Add(3 * keepalive))
			var m joinMsg
			if w.Recv(&m) != nil {
				return
			}
			select {
			case msgs <- m:
			case <-done:
				return
			case <-lctx.Done():
				return
			}
		}
	}()

	waited := false
	t := time.NewTicker(keepalive)
	defer t.Stop()
	for {
		changed := s.fsm.Changed()
		a := s.admission(claim)
		switch a.verdict {
		case verdictRefused:
			refuse(a.why)
			return
		case verdictClone:
			s.log.Error("two nodes with one node fingerprint", "node", claim.Name, "from", from, "fingerprint", claim.Fingerprint, "reason", a.why)
			refuse(a.why)
			return
		case verdictBusy:
			_ = w.Send(joinState{State: stateRetry, Message: a.why})
			return
		case verdictApproved:
			s.admit(w, claim, csr, a.by, waited, from, msgs, gone, lctx)
			return
		default:
			if !a.pending {
				p := cluster.Pending{Claim: claim, Since: s.n.d.Now().UTC()}
				if err := s.apply(cluster.TypePending, s.n.cfg.Name, p); err != nil {
					_ = w.Send(joinState{State: stateRetry, Message: "the cluster could not record the request: " + err.Error()})
					return
				}
				s.log.OK("node waiting for approval", "node", claim.Name, "role", claim.Role, "address", claim.Address, "mac", claim.MAC, "fingerprint", claim.Fingerprint, "hostname", claim.Hostname)
				continue
			}
			s.mu.Lock()
			s.asked[claim.Name] = time.Now()
			s.mu.Unlock()
			if !waited {
				waited = true
				if w.Send(joinState{State: statePending}) != nil {
					return
				}
			}
		}
		select {
		case <-changed:
		case <-msgs:
			s.mu.Lock()
			s.asked[claim.Name] = time.Now()
			s.mu.Unlock()
		case <-gone:
			return
		case <-lctx.Done():
			return
		case <-t.C:
			if w.Send(joinState{State: statePending}) != nil {
				return
			}
		}
	}
}

// admit signs an approved director's certificate, waits for it to listen as a
// member, records its new member identity - removing its old one - and adds it
// to Raft as a non-voter; it is promoted once it has caught up.
func (s *session) admit(w *wire.Conn, claim cluster.Claim, csr *x509.CertificateRequest, by string, waited bool, from netip.Addr, msgs <-chan joinMsg, gone <-chan struct{}, lctx context.Context) {
	a, err := s.signer()
	if err != nil {
		_ = w.Send(joinState{State: stateRetry, Message: err.Error()})
		return
	}
	cert, der, err := a.Sign("director", claim.Name, []netip.Addr{from, s.n.cfg.ControlVIP}, csr.PublicKey, s.n.d.Now(), ca.NodeLifetime)
	if err != nil {
		_ = w.Send(joinState{State: stateRetry, Message: err.Error()})
		return
	}
	if w.Send(joinState{State: stateApproved, By: by, Before: !waited, Cert: der, CA: s.id.CA.Raw}) != nil {
		return
	}
	var ready joinMsg
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	for ready.Op != "ready" {
		select {
		case ready = <-msgs:
		case <-gone:
			return
		case <-lctx.Done():
			return
		case <-timeout.C:
			return
		}
	}
	ap, err := netip.ParseAddrPort(ready.RaftAddr)
	if err != nil || ap.Addr().Unmap() != from || !strings.HasPrefix(ready.RaftID, claim.Name+"-") {
		_ = w.Send(joinState{State: stateRefused, Message: "its member address or identity is not its own"})
		return
	}
	var old string
	s.fsm.Read(func(st *cluster.State) {
		if n := st.Nodes[claim.Name]; n != nil {
			old = n.RaftID
		}
	})
	if err := s.apply(cluster.TypeAdmit, s.n.cfg.Name, cluster.Admit{Claim: claim, RaftID: ready.RaftID, RaftAddr: ready.RaftAddr, Cert: certRecord(claim.Name, cert)}); err != nil {
		_ = w.Send(joinState{State: stateRetry, Message: "the cluster could not record the member: " + err.Error()})
		return
	}
	if old != "" && old != ready.RaftID {
		if err := s.r.RemoveServer(raft.ServerID(old), 0, 5*time.Second).Error(); err == nil {
			s.log.OK("old member identity removed", "node", claim.Name, "member", old)
		}
	}
	f := s.r.AddNonvoter(raft.ServerID(ready.RaftID), raft.ServerAddress(ready.RaftAddr), 0, 10*time.Second)
	if err := f.Error(); err != nil {
		_ = w.Send(joinState{State: stateRetry, Message: "the cluster could not add the member: " + err.Error()})
		return
	}
	_ = w.Send(joinState{State: stateAdded, Index: f.Index()})
	if waited {
		s.log.OK("director joined", "node", claim.Name, "approved_by", by, "member", ready.RaftID, "address", from)
	} else {
		s.log.OK("director rejoined: approved before", "node", claim.Name, "approved_by", by, "member", ready.RaftID, "address", from)
	}
}
