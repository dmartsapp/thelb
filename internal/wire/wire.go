// Package wire is how theLB's nodes talk to one another: TLS 1.3 on the control
// port, where ALPN picks what a connection is for -
//
//   - thelb-join/1: a node asking to join, on the control VIP. The server shows
//     its certificate chain; neither side trusts the other until both have
//     proved they know the role's secret, with an HMAC over the connection's
//     exported keying material - bound to that one connection, and never the
//     secret itself.
//   - thelb-raft/1: Raft, between directors, with mutual TLS.
//   - thelb-api/1: requests between members - check-ins, approvals, renewals -
//     with mutual TLS.
//
// Messages are JSON, one per line.
package wire

import (
	"bufio"
	"crypto"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/dmartsapp/thelb/internal/ca"
	"github.com/dmartsapp/thelb/internal/secret"
)

// The protocols on the control port.
const (
	ALPNJoin = "thelb-join/1"
	ALPNRaft = "thelb-raft/1"
	ALPNAPI  = "thelb-api/1"
)

// Version is the version of the messages.
const Version = 1

// MaxLine is the longest message.
const MaxLine = 1 << 20

// HandshakeTimeout bounds a TLS handshake.
const HandshakeTimeout = 5 * time.Second

const exporterLabel = "EXPORTER-thelb-join-proof"

// The two sides of a proof: each proves with its own label, so one side's
// proof can never be sent back as the other's.
const (
	SideJoiner  = "joiner"
	SideCluster = "cluster"
)

// Proof is a side's proof that it knows s, bound to the connection cs.
func Proof(s secret.Secret, side string, cs tls.ConnectionState) ([]byte, error) {
	if cs.Version != tls.VersionTLS13 {
		return nil, errors.New("a proof needs TLS 1.3")
	}
	ekm, err := cs.ExportKeyingMaterial(exporterLabel, nil, 32)
	if err != nil {
		return nil, fmt.Errorf("exporting the connection's keying material: %w", err)
	}
	mac := hmac.New(sha256.New, s.Bytes())
	mac.Write([]byte("thelb join proof v1\x00" + side + "\x00"))
	mac.Write(ekm)
	return mac.Sum(nil), nil
}

// CheckProof is whether got is side's proof, on cs, of one of the secrets.
func CheckProof(secrets []secret.Secret, side string, cs tls.ConnectionState, got []byte) bool {
	ok := false
	for _, s := range secrets {
		want, err := Proof(s, side, cs)
		if err == nil && hmac.Equal(want, got) {
			ok = true // no early return: the time taken does not say which matched
		}
	}
	return ok
}

// Identity is a member's TLS identity: its certificate, renewed while it runs,
// and the cluster's CA.
type Identity struct {
	CA      *x509.Certificate
	Cluster string
	pool    *x509.CertPool
	// Revoked is whether a serial is revoked; nil: none is.
	Revoked func(serial string) bool

	mu   sync.RWMutex
	cert *tls.Certificate
}

// NewIdentity is an identity in the cluster of the CA, without a certificate
// yet.
func NewIdentity(caCert *x509.Certificate) (*Identity, error) {
	cluster, ok := ca.ClusterOf(caCert)
	if !ok {
		return nil, errors.New("not a theLB CA")
	}
	p := x509.NewCertPool()
	p.AddCert(caCert)
	return &Identity{CA: caCert, Cluster: cluster, pool: p}, nil
}

// SetCertificate installs the member's certificate; connections from now on
// present it, with the CA's.
func (id *Identity) SetCertificate(key crypto.Signer, der []byte) error {
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: id.pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return fmt.Errorf("the certificate is not the cluster CA's: %w", err)
	}
	c := &tls.Certificate{Certificate: [][]byte{der, id.CA.Raw}, PrivateKey: key, Leaf: leaf}
	id.mu.Lock()
	id.cert = c
	id.mu.Unlock()
	return nil
}

// Leaf is the member's certificate, or nil.
func (id *Identity) Leaf() *x509.Certificate {
	id.mu.RLock()
	defer id.mu.RUnlock()
	if id.cert == nil {
		return nil
	}
	return id.cert.Leaf
}

func (id *Identity) current() (*tls.Certificate, error) {
	id.mu.RLock()
	defer id.mu.RUnlock()
	if id.cert == nil {
		return nil, errors.New("no certificate yet")
	}
	return id.cert, nil
}

// checkPeer accepts a verified chain's leaf if it is a member of this cluster,
// in one of roles, and not revoked.
func (id *Identity) checkPeer(cs tls.ConnectionState, roles ...string) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("no certificate")
	}
	leaf := cs.PeerCertificates[0]
	p, ok := ca.PeerOf(leaf)
	if !ok || p.Cluster != id.Cluster || !slices.Contains(roles, p.Role) {
		return errors.New("not a member of this cluster in that role")
	}
	if id.Revoked != nil && id.Revoked(ca.Serial(leaf)) {
		return errors.New("the certificate is revoked")
	}
	return nil
}

// ServerConfig is the TLS configuration of a listener serving the protocols in
// allowed. A join is served without a client certificate; anything else needs a
// director's.
func (id *Identity) ServerConfig(allowed ...string) *tls.Config {
	get := func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return id.current() }
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: get,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			proto := ""
			for _, p := range hello.SupportedProtos {
				if slices.Contains(allowed, p) {
					proto = p
					break
				}
			}
			if proto == "" {
				return nil, errors.New("no protocol this listener serves")
			}
			c := &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: get, NextProtos: []string{proto}}
			if proto != ALPNJoin {
				c.ClientAuth = tls.RequireAndVerifyClientCert
				c.ClientCAs = id.pool
				c.VerifyConnection = func(cs tls.ConnectionState) error { return id.checkPeer(cs, "director") }
			}
			return c, nil
		},
	}
}

// ClientConfig is the TLS configuration for proto to the director at ip.
func (id *Identity) ClientConfig(proto string, ip netip.Addr) *tls.Config {
	return &tls.Config{
		MinVersion:           tls.VersionTLS13,
		NextProtos:           []string{proto},
		RootCAs:              id.pool,
		ServerName:           ip.Unmap().String(),
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return id.current() },
		VerifyConnection:     func(cs tls.ConnectionState) error { return id.checkPeer(cs, "director") },
	}
}

// Cluster is what a joiner learns of the cluster from the TLS handshake - to be
// trusted only once the cluster has proved it knows the secret.
type Cluster struct {
	CA     *x509.Certificate
	Name   string
	Leader string // the name in the leader's certificate
}

// JoinConfig is the TLS configuration for a join to the control VIP. It checks
// that the server's chain is a theLB cluster's, for the VIP, and records it in
// got; it cannot check that it is the right cluster - the proofs do that.
func JoinConfig(vip netip.Addr, got *Cluster) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{ALPNJoin},
		InsecureSkipVerify: true, // verified below, against the CA in the chain
		VerifyConnection: func(cs tls.ConnectionState) error {
			chain := cs.PeerCertificates
			if len(chain) < 2 {
				return errors.New("the server sent no CA")
			}
			leaf, caCert := chain[0], chain[len(chain)-1]
			name, ok := ca.ClusterOf(caCert)
			if !ok || !caCert.IsCA {
				return errors.New("the server's chain has no theLB CA")
			}
			p := x509.NewCertPool()
			p.AddCert(caCert)
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: p, DNSName: vip.Unmap().String(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
				return fmt.Errorf("the server's certificate: %w", err)
			}
			peer, ok := ca.PeerOf(leaf)
			if !ok || peer.Cluster != name || peer.Role != "director" {
				return errors.New("the server is not a director of the cluster its CA names")
			}
			if got != nil {
				*got = Cluster{CA: caCert, Name: name, Leader: peer.Name}
			}
			return nil
		},
	}
}

// PeerName is the name in a mutual-TLS connection's client or server
// certificate.
func PeerName(c *tls.Conn) (ca.Peer, bool) {
	cs := c.ConnectionState()
	if len(cs.PeerCertificates) == 0 {
		return ca.Peer{}, false
	}
	return ca.PeerOf(cs.PeerCertificates[0])
}

// Conn sends and receives messages, one JSON document per line. Sending is
// safe from several goroutines; receiving is for one.
type Conn struct {
	net.Conn
	r  *bufio.Reader
	mu sync.Mutex
}

// NewConn wraps c.
func NewConn(c net.Conn) *Conn { return &Conn{Conn: c, r: bufio.NewReaderSize(c, 64<<10)} }

// Send writes one message.
func (c *Conn) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.Write(append(b, '\n'))
	return err
}

// Recv reads one message into v.
func (c *Conn) Recv(v any) error {
	var line []byte
	for {
		part, err := c.r.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > MaxLine {
			return errors.New("a message is too long")
		}
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
	return json.Unmarshal(line, v)
}

// Mux serves the control port's protocols on listeners: each connection's TLS
// handshake, then the handler for the protocol it chose.
type Mux struct {
	// Handlers by protocol; a handler owns the connection.
	Handlers map[string]func(*tls.Conn)

	wg sync.WaitGroup
}

// Serve accepts connections on ln until it is closed.
func (m *Mux) Serve(ln net.Listener, cfg *tls.Config) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			tc := tls.Server(c, cfg)
			_ = tc.SetDeadline(time.Now().Add(HandshakeTimeout))
			if err := tc.Handshake(); err != nil {
				_ = c.Close()
				return
			}
			_ = tc.SetDeadline(time.Time{})
			h := m.Handlers[tc.ConnectionState().NegotiatedProtocol]
			if h == nil {
				_ = tc.Close()
				return
			}
			h(tc)
		}()
	}
}

// Wait waits for every handler to return.
func (m *Mux) Wait() { m.wg.Wait() }
