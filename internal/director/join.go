package director

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/dmartsapp/thelb/internal/ca"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/secret"
	"github.com/dmartsapp/thelb/internal/wire"
)

// The join, on the wire. A joiner sends joinHello; the cluster answers
// joinReply, with its own proof if the joiner's was right; the joiner sends
// joinRequest, then joinMsg "here" while it waits and "ready" once it is
// listening as a member; the cluster sends joinState as things happen.
type (
	joinHello struct {
		V     int    `json:"v"`
		Role  string `json:"role"`
		Proof []byte `json:"proof"`
	}
	joinReply struct {
		V         int    `json:"v"`
		OK        bool   `json:"ok"`
		Retry     bool   `json:"retry,omitempty"`
		Error     string `json:"error,omitempty"`
		Proof     []byte `json:"proof,omitempty"`
		ClusterID string `json:"cluster_id,omitempty"`
	}
	joinRequest struct {
		Claim cluster.Claim `json:"claim"`
		CSR   []byte        `json:"csr"`
	}
	joinMsg struct {
		Op       string `json:"op"` // "here" or "ready"
		RaftID   string `json:"raft_id,omitempty"`
		RaftAddr string `json:"raft_addr,omitempty"`
	}
	joinState struct {
		State   string `json:"state"` // pending, retry, refused, approved, added
		Message string `json:"message,omitempty"`
		By      string `json:"by,omitempty"`
		Before  bool   `json:"before,omitempty"` // approved at an earlier join
		Cert    []byte `json:"cert,omitempty"`
		CA      []byte `json:"ca,omitempty"`
		Index   uint64 `json:"index,omitempty"`
	}
)

// The join's states.
const (
	statePending  = "pending"
	stateRetry    = "retry"
	stateRefused  = "refused"
	stateApproved = "approved"
	stateAdded    = "added"
)

// errNoAnswer is a join that found nothing on the control VIP.
var errNoAnswer = errors.New("no cluster answers")

// errStop is a join that cannot succeed by trying again: the director stops.
type errStop struct{ msg string }

func (e errStop) Error() string { return e.msg }

// create creates a cluster: an ID, a CA, the director's own key and
// certificate - all in memory - and a Raft configuration of itself alone. The
// cluster's state is written when it first leads.
func (n *node) create() (*session, error) {
	now := n.d.Now()
	id, err := randomHex(4)
	if err != nil {
		return nil, err
	}
	authority, err := ca.New(n.cfg.Cluster, now)
	if err != nil {
		return nil, err
	}
	key, err := ca.NewKey()
	if err != nil {
		return nil, err
	}
	cert, der, err := authority.Sign("director", n.cfg.Name, []netip.Addr{n.addr.Prefix.Addr(), n.cfg.ControlVIP}, &key.PublicKey, now, ca.NodeLifetime)
	if err != nil {
		return nil, err
	}
	keyDER, err := authority.KeyDER()
	if err != nil {
		return nil, err
	}
	raftID, err := n.memberID()
	if err != nil {
		return nil, err
	}
	creating := &cluster.Create{
		Info: cluster.Info{Name: n.cfg.Cluster, ID: id, Created: now.UTC(), CACert: authority.DER, CAKey: keyDER},
		Node: cluster.Node{Claim: n.claim, ApprovedBy: "bootstrap", ApprovedAt: now.UTC(), Admitted: now.UTC()},
		Cert: certRecord(n.cfg.Name, cert),
	}
	n.log.OK("creating the cluster: nothing answers on the control VIP", "cluster", n.cfg.Cluster, "vip", n.cfg.ControlVIP, "port", n.cfg.ControlPort)
	return newSession(n, raftID, key, der, authority.Cert, creating)
}

// memberID is a new member identity: the node's name and a random suffix.
func (n *node) memberID() (string, error) {
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}
	return n.cfg.Name + "-" + suffix, nil
}

// join asks the cluster on the control VIP to let this director in, and waits
// - for approval, if it needs one - until it is a member. It returns nil and no
// error if ctx ends first.
func (n *node) join(ctx context.Context) (*session, error) {
	key, err := ca.NewKey()
	if err != nil {
		return nil, err
	}
	csr, err := ca.Request(key, n.cfg.Name)
	if err != nil {
		return nil, err
	}
	n.setPhase("joining")
	said := map[string]time.Time{} // what was last logged, by message, and when
	say := func(level, msg string, kv ...any) {
		if time.Since(said[msg]) < time.Minute {
			return
		}
		said[msg] = time.Now()
		if level == "WARN" {
			n.log.Warn(msg, kv...)
		} else {
			n.log.OK(msg, kv...)
		}
	}
	var silentSince time.Time
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, nil
			case <-time.After(joinRetry):
			}
		}
		s, err := n.joinOnce(ctx, key, csr, say)
		switch {
		case ctx.Err() != nil:
			if s != nil {
				s.close(false)
			}
			return nil, nil
		case err == nil:
			return s, nil
		case errors.As(err, new(errStop)):
			return nil, err
		case errors.Is(err, errNoAnswer):
			// The cluster that answered is gone. A director that may create one
			// looks again, as at its start; one that may not keeps asking.
			if silentSince.IsZero() {
				silentSince = time.Now()
			}
			if n.cfg.Bootstrap && time.Since(silentSince) > 2*probeWindow {
				return nil, fmt.Errorf("%w: the cluster stopped answering on the control VIP", errRejoin)
			}
			say("WARN", "joining the cluster: trying again", "error", err)
		default:
			silentSince = time.Time{}
			say("WARN", "joining the cluster: trying again", "error", err)
		}
	}
}

func (n *node) joinOnce(ctx context.Context, key *ecdsa.PrivateKey, csr []byte, say func(string, string, ...any)) (*session, error) {
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	c, err := n.net.DialVIP(dctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("%w on %s port %d", errNoAnswer, n.cfg.ControlVIP, n.cfg.ControlPort)
	}
	var cl wire.Cluster
	tc := tls.Client(c, wire.JoinConfig(n.cfg.ControlVIP, &cl))
	defer func() { _ = tc.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = tc.Close() })
	defer stop()
	_ = tc.SetDeadline(time.Now().Add(wire.HandshakeTimeout))
	if err := tc.Handshake(); err != nil {
		return nil, fmt.Errorf("the TLS handshake on the control VIP: %w", err)
	}
	w := wire.NewConn(tc)
	cs := tc.ConnectionState()
	n.mu.Lock()
	dirSecret := n.dirSecret
	n.mu.Unlock()
	proof, err := wire.Proof(dirSecret, wire.SideJoiner, cs)
	if err != nil {
		return nil, err
	}
	if err := w.Send(joinHello{V: wire.Version, Role: "director", Proof: proof}); err != nil {
		return nil, err
	}
	var hr joinReply
	if err := w.Recv(&hr); err != nil {
		return nil, fmt.Errorf("the cluster's answer: %w", err)
	}
	if !hr.OK {
		if hr.Retry {
			return nil, errors.New(hr.Error)
		}
		return nil, errStop{fmt.Sprintf("the cluster at %s did not accept the secret in %s: it is out of date, or belongs to another cluster. On a director, run `sudo thelb admin secret show --role director`, put what it prints in %s, and start this director again.",
			netip.AddrPortFrom(n.cfg.ControlVIP, n.cfg.ControlPort), n.cfg.SecretFile, n.cfg.SecretFile)}
	}
	if !wire.CheckProof([]secret.Secret{dirSecret}, wire.SideCluster, cs, hr.Proof) {
		return nil, errStop{fmt.Sprintf("what answers on %s did not prove it knows the directors' secret: it is not this cluster, and this director does not join it",
			netip.AddrPortFrom(n.cfg.ControlVIP, n.cfg.ControlPort))}
	}
	// From here, the cluster is the real one, and its CA is trusted.
	n.mu.Lock()
	n.joining, n.joinID = &cl, hr.ClusterID
	n.mu.Unlock()
	if err := w.Send(joinRequest{Claim: n.claim, CSR: csr}); err != nil {
		return nil, err
	}

	// Say we are here, while we wait.
	alive, quit := context.WithCancel(ctx)
	defer quit()
	go func() {
		t := time.NewTicker(keepalive)
		defer t.Stop()
		for {
			select {
			case <-alive.Done():
				return
			case <-t.C:
				if w.Send(joinMsg{Op: "here"}) != nil {
					return
				}
			}
		}
	}()

	var s *session
	fail := func(err error) (*session, error) {
		if s != nil {
			s.close(false)
		}
		return nil, err
	}
	for {
		_ = tc.SetReadDeadline(time.Now().Add(3 * keepalive))
		var st joinState
		if err := w.Recv(&st); err != nil {
			return fail(fmt.Errorf("the connection to the cluster ended: %w", err))
		}
		switch st.State {
		case statePending:
			n.setPhase("waiting for approval")
			say("OK", "waiting for approval", "cluster", cl.Name, "node", n.cfg.Name, "fingerprint", n.claim.Fingerprint, "address", n.claim.Address, "mac", n.claim.MAC)
		case stateRetry:
			return fail(errors.New(st.Message))
		case stateRefused:
			return fail(errStop{fmt.Sprintf("the cluster %s refused this director: %s", cl.Name, st.Message)})
		case stateApproved:
			if s != nil {
				return fail(errors.New("approved twice"))
			}
			caCert, err := x509.ParseCertificate(st.CA)
			if err != nil || !caCert.Equal(cl.CA) {
				return fail(errStop{"the cluster sent a CA that is not the one it showed: this director does not join it"})
			}
			raftID, err := n.memberID()
			if err != nil {
				return fail(err)
			}
			if s, err = newSession(n, raftID, key, st.Cert, caCert, nil); err != nil {
				return fail(err)
			}
			if err := w.Send(joinMsg{Op: "ready", RaftID: raftID, RaftAddr: s.addr}); err != nil {
				return fail(err)
			}
			if st.Before {
				n.log.OK("approved before: rejoining", "cluster", cl.Name, "approved_by", st.By)
			} else {
				n.log.OK("approved", "cluster", cl.Name, "by", st.By)
			}
		case stateAdded:
			if s == nil {
				return fail(errors.New("added before it was approved"))
			}
			n.log.OK("joined the cluster", "cluster", cl.Name, "id", hr.ClusterID, "member", s.raftID, "leader", cl.Leader)
			return s, nil
		default:
			return fail(fmt.Errorf("the cluster sent %q, which this director does not know", st.State))
		}
	}
}

// claimProblem is what is wrong with a claim, from the connection's address; ""
// when nothing is.
func claimProblem(c cluster.Claim, from netip.Addr) string {
	switch {
	case !nameOK(c.Name):
		return fmt.Sprintf("the name %q is not a node's name", c.Name)
	case c.Role != "director":
		return "only directors join this way"
	case len(c.Fingerprint) != 16 || strings.Trim(c.Fingerprint, "0123456789abcdef") != "":
		return "the node fingerprint is not one"
	}
	a, err := netip.ParseAddr(c.Address)
	if err != nil || a.Unmap() != from.Unmap() {
		return fmt.Sprintf("the connection comes from %s, not from the address claimed, %s", from, c.Address)
	}
	return ""
}

func nameOK(name string) bool {
	if name == "" || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	return strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789-") == ""
}
