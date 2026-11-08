// Package join is how a node asks a cluster to let it in, on the control VIP:
// directors and backends alike, each with its role's secret. The joiner sends
// Hello with its proof; the cluster answers Reply, with its own proof if the
// joiner's was right; the joiner sends Request, then Msg "here" while it waits
// and "ready" once it is set up as a member; the cluster sends State as things
// happen.
package join

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/secret"
	"github.com/dmartsapp/thelb/internal/wire"
)

// The messages.
type (
	// Hello is the joiner's first message.
	Hello struct {
		V     int    `json:"v"`
		Role  string `json:"role"`
		Proof []byte `json:"proof"`
	}
	// Reply is the cluster's answer to it.
	Reply struct {
		V         int    `json:"v"`
		OK        bool   `json:"ok"`
		Retry     bool   `json:"retry,omitempty"`
		Error     string `json:"error,omitempty"`
		Proof     []byte `json:"proof,omitempty"`
		ClusterID string `json:"cluster_id,omitempty"`
	}
	// Request is what the joiner asks for, once both have proved the secret.
	Request struct {
		Claim cluster.Claim `json:"claim"`
		CSR   []byte        `json:"csr"`
	}
	// Msg is the joiner's: "here" while it waits, "ready" once it is set up.
	Msg struct {
		Op       string `json:"op"`
		RaftID   string `json:"raft_id,omitempty"` // a member identity: a director's Raft ID, a backend's session
		RaftAddr string `json:"raft_addr,omitempty"`
	}
	// State is the cluster's, as things happen.
	State struct {
		State   string `json:"state"`
		Message string `json:"message,omitempty"`
		By      string `json:"by,omitempty"`
		Before  bool   `json:"before,omitempty"` // approved at an earlier join
		Cert    []byte `json:"cert,omitempty"`
		CA      []byte `json:"ca,omitempty"`
		Index   uint64 `json:"index,omitempty"`
	}
)

// The states.
const (
	Pending  = "pending"
	Retry    = "retry"
	Refused  = "refused"
	Approved = "approved"
	Added    = "added"
)

// Keepalive is how often both ends of a pending join say they are there.
var Keepalive = 10 * time.Second

// ErrNoAnswer is a join that found nothing on the control VIP.
var ErrNoAnswer = errors.New("no cluster answers")

// Stop is a join that cannot succeed by trying again: the node stops.
type Stop struct{ Msg string }

func (e Stop) Error() string { return e.Msg }

// Asker asks one cluster, once.
type Asker struct {
	VIP        netip.AddrPort
	Role       string // director or backend
	Secret     secret.Secret
	SecretFile string // named in the message when the secret is out of date
	Claim      cluster.Claim
	CSR        []byte
	Dial       func(ctx context.Context) (net.Conn, error)
	// Proven is called once both sides have proved the secret; Waiting each
	// time the cluster says the node waits for approval.
	Proven  func(cl wire.Cluster, clusterID string)
	Waiting func()
}

// Approval is a join a director approved - now or before. Its connection stays
// open until Ready or Close.
type Approval struct {
	Cluster   wire.Cluster
	ClusterID string
	Cert      []byte
	CA        *x509.Certificate
	By        string
	Before    bool

	tc   *tls.Conn
	w    *wire.Conn
	quit context.CancelFunc
	once sync.Once
}

// Ask asks the cluster on the control VIP, and waits - through approval - until
// the cluster approves the node, or something ends the join.
func (a Asker) Ask(ctx context.Context) (*Approval, error) {
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	c, err := a.Dial(dctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("%w on %s", ErrNoAnswer, a.VIP)
	}
	var cl wire.Cluster
	tc := tls.Client(c, wire.JoinConfig(a.VIP.Addr(), &cl))
	alive, quit := context.WithCancel(ctx)
	ap := &Approval{tc: tc, w: wire.NewConn(tc), quit: quit}
	ok := false
	defer func() {
		if !ok {
			ap.Close()
		}
	}()
	context.AfterFunc(alive, func() { _ = tc.Close() })
	_ = tc.SetDeadline(time.Now().Add(wire.HandshakeTimeout))
	if err := tc.Handshake(); err != nil {
		return nil, fmt.Errorf("the TLS handshake on the control VIP: %w", err)
	}
	cs := tc.ConnectionState()
	proof, err := wire.Proof(a.Secret, wire.SideJoiner, cs)
	if err != nil {
		return nil, err
	}
	if err := ap.w.Send(Hello{V: wire.Version, Role: a.Role, Proof: proof}); err != nil {
		return nil, err
	}
	var hr Reply
	if err := ap.w.Recv(&hr); err != nil {
		return nil, fmt.Errorf("the cluster's answer: %w", err)
	}
	if !hr.OK {
		if hr.Retry {
			return nil, errors.New(hr.Error)
		}
		return nil, Stop{fmt.Sprintf("the cluster at %s did not accept the secret in %s: it is out of date, or belongs to another cluster. On a director, run `sudo thelb admin secret show --role %s`, put what it prints in %s, and start this %s again.",
			a.VIP, a.SecretFile, a.Role, a.SecretFile, a.Role)}
	}
	if !wire.CheckProof([]secret.Secret{a.Secret}, wire.SideCluster, cs, hr.Proof) {
		return nil, Stop{fmt.Sprintf("what answers on %s did not prove it knows the %ss' secret: it is not this cluster, and this %s does not join it", a.VIP, a.Role, a.Role)}
	}
	// From here, the cluster is the real one, and its CA is trusted.
	ap.Cluster, ap.ClusterID = cl, hr.ClusterID
	if a.Proven != nil {
		a.Proven(cl, hr.ClusterID)
	}
	_ = tc.SetDeadline(time.Time{})
	if err := ap.w.Send(Request{Claim: a.Claim, CSR: a.CSR}); err != nil {
		return nil, err
	}
	go func() {
		t := time.NewTicker(Keepalive)
		defer t.Stop()
		for {
			select {
			case <-alive.Done():
				return
			case <-t.C:
				if ap.w.Send(Msg{Op: "here"}) != nil {
					return
				}
			}
		}
	}()
	for {
		st, err := ap.recv()
		if err != nil {
			return nil, err
		}
		switch st.State {
		case Pending:
			if a.Waiting != nil {
				a.Waiting()
			}
		case Retry:
			return nil, errors.New(st.Message)
		case Refused:
			return nil, Stop{fmt.Sprintf("the cluster %s refused this %s: %s", cl.Name, a.Role, st.Message)}
		case Approved:
			caCert, err := x509.ParseCertificate(st.CA)
			if err != nil || !caCert.Equal(cl.CA) {
				return nil, Stop{fmt.Sprintf("the cluster sent a CA that is not the one it showed: this %s does not join it", a.Role)}
			}
			ap.Cert, ap.CA, ap.By, ap.Before = st.Cert, caCert, st.By, st.Before
			ok = true
			return ap, nil
		default:
			return nil, fmt.Errorf("the cluster sent %q, which this version does not know", st.State)
		}
	}
}

func (ap *Approval) recv() (State, error) {
	_ = ap.tc.SetReadDeadline(time.Now().Add(3 * Keepalive))
	var st State
	if err := ap.w.Recv(&st); err != nil {
		return st, fmt.Errorf("the connection to the cluster ended: %w", err)
	}
	return st, nil
}

// Ready tells the cluster the node is set up as member - with its Raft
// address, for a director - and waits until the cluster has added it. It
// closes the join's connection.
func (ap *Approval) Ready(member, raftAddr string) (uint64, error) {
	defer ap.Close()
	if err := ap.w.Send(Msg{Op: "ready", RaftID: member, RaftAddr: raftAddr}); err != nil {
		return 0, err
	}
	st, err := ap.recv()
	if err != nil {
		return 0, err
	}
	if st.State != Added {
		if st.State == Refused {
			return 0, Stop{"the cluster refused the member: " + st.Message}
		}
		return 0, errors.New(st.Message)
	}
	return st.Index, nil
}

// Close ends the join.
func (ap *Approval) Close() {
	ap.once.Do(func() {
		ap.quit()
		_ = ap.tc.Close()
	})
}

// NameOK is whether name is a node's name.
func NameOK(name string) bool {
	if name == "" || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// Problem is what is wrong with a claim to join as role, from the connection's
// address; "" when nothing is.
func Problem(c cluster.Claim, role string, from netip.Addr) string {
	switch {
	case !NameOK(c.Name):
		return fmt.Sprintf("the name %q is not a node's name", c.Name)
	case c.Role != role:
		return fmt.Sprintf("a %s's secret cannot join a %s", role, c.Role)
	case len(c.Fingerprint) != 16 || !hex(c.Fingerprint):
		return "the node fingerprint is not one"
	}
	a, err := netip.ParseAddr(c.Address)
	if err != nil || a.Unmap() != from.Unmap() {
		return fmt.Sprintf("the connection comes from %s, not from the address claimed, %s", from, c.Address)
	}
	return ""
}

func hex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
