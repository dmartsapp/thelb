package wire

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/ca"
	"github.com/dmartsapp/thelb/internal/secret"
)

var vip = netip.MustParseAddr("127.0.0.1")

type member struct {
	id   *Identity
	cert *ca.Certificate
}

func newMember(t *testing.T, a *ca.Authority, name string) member {
	t.Helper()
	c, err := a.Issue("director", name, []netip.Addr{vip}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, err := NewIdentity(a.Cert)
	if err != nil {
		t.Fatal(err)
	}
	if err := id.SetCertificate(c.Key, c.DER); err != nil {
		t.Fatal(err)
	}
	return member{id: id, cert: c}
}

// serve runs a mux on a loopback listener and returns its address.
func serve(t *testing.T, m *Mux, cfg *tls.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go m.Serve(ln, cfg)
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func TestTheJoinProofsBindToOneConnection(t *testing.T) {
	a, _ := ca.New("prod", time.Now())
	leader := newMember(t, a, "lb-1")
	s, _ := secret.Generate()
	wrong, _ := secret.Generate()

	type result struct {
		joinerOK bool
		proof    []byte
	}
	results := make(chan result, 4)
	m := &Mux{Handlers: map[string]func(*tls.Conn){ALPNJoin: func(c *tls.Conn) {
		defer func() { _ = c.Close() }()
		w := NewConn(c)
		var got []byte
		if err := w.Recv(&got); err != nil {
			return
		}
		ok := CheckProof([]secret.Secret{s}, SideJoiner, c.ConnectionState(), got)
		p, _ := Proof(s, SideCluster, c.ConnectionState())
		results <- result{joinerOK: ok, proof: p}
		_ = w.Send(p)
	}}}
	addr := serve(t, m, leader.id.ServerConfig(ALPNJoin))

	join := func(with secret.Secret) (Cluster, bool, []byte) {
		var cl Cluster
		c, err := tls.Dial("tcp", addr, JoinConfig(vip, &cl))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		w := NewConn(c)
		p, err := Proof(with, SideJoiner, c.ConnectionState())
		if err != nil {
			t.Fatal(err)
		}
		_ = w.Send(p)
		var back []byte
		if err := w.Recv(&back); err != nil {
			t.Fatal(err)
		}
		return cl, CheckProof([]secret.Secret{with}, SideCluster, c.ConnectionState(), back), p
	}

	cl, clusterOK, first := join(s)
	r := <-results
	if !r.joinerOK || !clusterOK {
		t.Errorf("the right secret: joiner accepted %v, cluster accepted %v", r.joinerOK, clusterOK)
	}
	if cl.Name != "prod" || cl.Leader != "lb-1" || !cl.CA.Equal(a.Cert) {
		t.Errorf("the cluster learned %+v", cl)
	}

	_, clusterOK, _ = join(wrong)
	r = <-results
	if r.joinerOK || clusterOK {
		t.Error("a wrong secret was accepted on one side or the other")
	}

	// A proof recorded on one connection is no good on another.
	c, err := tls.Dial("tcp", addr, JoinConfig(vip, nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = NewConn(c).Send(first)
	if r = <-results; r.joinerOK {
		t.Error("a proof was replayed on a second connection")
	}
	_ = c.Close()
}

func TestTheJoinChecksTheServersChain(t *testing.T) {
	a, _ := ca.New("prod", time.Now())
	leader := newMember(t, a, "lb-1")
	m := &Mux{Handlers: map[string]func(*tls.Conn){ALPNJoin: func(c *tls.Conn) { _ = c.Close() }}}
	addr := serve(t, m, leader.id.ServerConfig(ALPNJoin))
	if c, err := tls.Dial("tcp", addr, JoinConfig(netip.MustParseAddr("127.0.0.2"), nil)); err == nil {
		_ = c.Close()
		t.Error("a certificate without the VIP was accepted for it")
	}

	// A server that is not theLB: a self-signed certificate.
	other := tlsServerSelfSigned(t)
	if c, err := tls.Dial("tcp", other, JoinConfig(vip, nil)); err == nil {
		_ = c.Close()
		t.Error("a server that is not theLB was taken for a cluster")
	}
}

func tlsServerSelfSigned(t *testing.T) string {
	a, _ := ca.New("x", time.Now())
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{a.DER}, PrivateKey: mustKey(t, a)}},
		NextProtos:   []string{ALPNJoin},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.(*tls.Conn).Handshake()
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

func mustKey(t *testing.T, a *ca.Authority) any {
	der, err := a.KeyDER()
	if err != nil {
		t.Fatal(err)
	}
	k, err := x509.ParseECPrivateKey(der)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRaftAndTheAPINeedADirectorsCertificate(t *testing.T) {
	a, _ := ca.New("prod", time.Now())
	lb1 := newMember(t, a, "lb-1")
	lb2 := newMember(t, a, "lb-2")
	revoked := map[string]bool{}
	lb1.id.Revoked = func(s string) bool { return revoked[s] }

	served := make(chan string, 4)
	m := &Mux{Handlers: map[string]func(*tls.Conn){
		ALPNRaft: func(c *tls.Conn) {
			p, _ := PeerName(c)
			served <- p.Name
			_ = c.Close()
		},
	}}
	addr := serve(t, m, lb1.id.ServerConfig(ALPNRaft, ALPNAPI))

	c, err := tls.Dial("tcp", addr, lb2.id.ClientConfig(ALPNRaft, vip))
	if err != nil {
		t.Fatal(err)
	}
	if got := <-served; got != "lb-2" {
		t.Errorf("served %q", got)
	}
	_ = c.Close()

	// Without a certificate.
	if c, err := tls.Dial("tcp", addr, &tls.Config{NextProtos: []string{ALPNRaft}, RootCAs: lb2.id.pool, ServerName: "127.0.0.1"}); err == nil {
		if _, rerr := c.Read(make([]byte, 1)); rerr == nil {
			t.Error("a connection without a certificate was served")
		}
		_ = c.Close()
	}
	// Another cluster's member.
	b, _ := ca.New("prod", time.Now())
	stranger := newMember(t, b, "lb-2")
	if c, err := tls.Dial("tcp", addr, stranger.id.ClientConfig(ALPNRaft, vip)); err == nil {
		if _, rerr := c.Read(make([]byte, 1)); rerr == nil {
			t.Error("another cluster's member was served")
		}
		_ = c.Close()
	}
	// A revoked certificate.
	revoked[ca.Serial(lb2.cert.Cert)] = true
	if c, err := tls.Dial("tcp", addr, lb2.id.ClientConfig(ALPNRaft, vip)); err == nil {
		if _, rerr := c.Read(make([]byte, 1)); rerr == nil {
			t.Error("a revoked certificate was served")
		}
		_ = c.Close()
	}
	// The join protocol is not served where it is not allowed.
	if c, err := tls.Dial("tcp", addr, JoinConfig(vip, nil)); err == nil {
		_ = c.Close()
		t.Error("a join was served on a listener without it")
	}
	select {
	case got := <-served:
		t.Errorf("served %q", got)
	default:
	}
}

func TestALongMessageIsRefused(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	go func() { _, _ = a.Write([]byte(`"` + strings.Repeat("x", MaxLine+10) + `"` + "\n")) }()
	var s string
	if err := NewConn(b).Recv(&s); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("a long message: %v", err)
	}
}
