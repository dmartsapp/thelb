// Command attack attacks a theLB cluster from outside, for the security lab
// (test/e2e-security.sh): the control port, the join, a member gone bad, the
// metrics endpoint, and a Kubernetes API server that lies. Each attack says
// what it expects of theLB, and prints
//
//	PASS <what>        theLB did what it should
//	FAIL <what>: why   it did not
//	INFO <what>        a measurement
//
// exiting 1 when anything failed. What the attacks do to the cluster as a
// whole - is it still up, still leading, still serving - the lab checks.
//
// It is inside theLB's module, so it speaks theLB's own protocols with theLB's
// own packages: a join done exactly as a node does it, then abused.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/ca"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/join"
	"github.com/dmartsapp/thelb/internal/secret"
	"github.com/dmartsapp/thelb/internal/wire"
)

var failed atomic.Bool

func pass(format string, a ...any) { fmt.Printf("PASS %s\n", fmt.Sprintf(format, a...)) }
func info(format string, a ...any) { fmt.Printf("INFO %s\n", fmt.Sprintf(format, a...)) }
func fail(format string, a ...any) {
	failed.Store(true)
	fmt.Printf("FAIL %s\n", fmt.Sprintf(format, a...))
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: attack <attack> [arguments]
  tcpflood  <addr> <connections> <hold>   hold connections open, saying nothing
  probe     <addr> <count>                connect and close, as fast as it can
  garbage   <addr>                        what is not theLB's protocol
  slow      <addr>                        handshakes that never finish, joins that say nothing
  join      <vip:port> <backend secret file> <director secret file>
                                          joins without the secret, and claims that lie
  joinflood <vip:port> <count>            joins with a wrong secret, all at once
  pending   <vip:port> <backend secret file> <count>
                                          joins with the secret, under new names
  insider   <vip:port> <backend secret file> <name> <dir> <node addr:port>
                                          a backend, approved, that turns bad
  revoked   <vip:port> <dir>              the same backend, after it was removed
  metrics   <addr>                        the metrics endpoint
  evilkube  <listen addr> <mode> <dir>    a Kubernetes API server that lies
  impostor  <listen addr> <dir>           a "cluster" without the secret, for a node to join`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 3 {
		usage()
	}
	args := os.Args[2:]
	need := func(n int) {
		if len(args) != n {
			usage()
		}
	}
	switch os.Args[1] {
	case "tcpflood":
		need(3)
		tcpflood(args[0], atoi(args[1]), dur(args[2]))
	case "probe":
		need(2)
		probe(args[0], atoi(args[1]))
	case "garbage":
		need(1)
		garbage(args[0])
	case "slow":
		need(1)
		slow(args[0])
	case "join":
		need(3)
		joins(ap(args[0]), mustSecret(args[1]), mustSecret(args[2]))
	case "joinflood":
		need(2)
		joinflood(ap(args[0]), atoi(args[1]))
	case "pending":
		need(3)
		pendingFlood(ap(args[0]), mustSecret(args[1]), atoi(args[2]))
	case "insider":
		need(5)
		insider(ap(args[0]), mustSecret(args[1]), args[2], args[3], ap(args[4]))
	case "revoked":
		need(2)
		revoked(ap(args[0]), args[1])
	case "metrics":
		need(1)
		metrics(args[0])
	case "evilkube":
		need(3)
		evilkube(args[0], args[1], args[2])
	case "impostor":
		need(2)
		impostor(ap(args[0]), args[1])
	default:
		usage()
	}
	if failed.Load() {
		os.Exit(1)
	}
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		usage()
	}
	return n
}

func dur(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		usage()
	}
	return d
}

func ap(s string) netip.AddrPort {
	a, err := netip.ParseAddrPort(s)
	if err != nil {
		usage()
	}
	return a
}

func mustSecret(path string) secret.Secret {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	s, err := secret.Parse(string(b))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	return s
}

// closedWithin says whether the server closes c - reading nothing from it or
// nothing more - within d, and how long it took.
func closedWithin(c net.Conn, d time.Duration) (bool, time.Duration) {
	start := time.Now()
	_ = c.SetReadDeadline(start.Add(d))
	buf := make([]byte, 4096)
	for {
		_, err := c.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return false, time.Since(start)
			}
			return true, time.Since(start)
		}
	}
}

// ---- the control port, before any proof ----

func tcpflood(addr string, n int, hold time.Duration) {
	var conns []net.Conn
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 200)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			c, err := net.DialTimeout("tcp", addr, 3*time.Second)
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}()
	}
	wg.Wait()
	info("tcpflood: %d of %d connections open, saying nothing, for %s", len(conns), n, hold)
	time.Sleep(hold)
	open := 0
	for _, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				open++ // still open
			}
		}
		_ = c.Close()
	}
	if hold > 6*time.Second && open > 0 {
		fail("tcpflood: %d connections that never began a handshake were still open after %s (the handshake timeout is %s)", open, hold, wire.HandshakeTimeout)
	} else {
		pass("tcpflood: every silent connection was closed by the server within the handshake timeout (%d still open after %s)", open, hold)
	}
}

func probe(addr string, n int) {
	ok := 0
	start := time.Now()
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			continue
		}
		ok++
		_ = c.Close()
	}
	info("probe: %d of %d connect-and-close in %s", ok, n, time.Since(start).Round(time.Millisecond))
	if ok < n*9/10 {
		fail("probe: the control port refused %d of %d connections", n-ok, n)
	} else {
		pass("probe: the control port took %d connect-and-close", ok)
	}
}

func garbage(addr string) {
	raw := func(what string, payload []byte) {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			fail("garbage %s: no connection: %v", what, err)
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = c.Write(payload)
		closed, took := closedWithin(c, 8*time.Second)
		if !closed {
			fail("garbage %s: still open after %s", what, took.Round(time.Millisecond))
			return
		}
		pass("garbage %s: closed after %s", what, took.Round(time.Millisecond))
	}
	random := make([]byte, 64<<10)
	_, _ = rand.Read(random)
	raw("64 KiB of random bytes", random)
	raw("an HTTP request", []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	raw("an HTTP/2 preface", []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
	raw("a TLS record header promising 16 MiB", []byte{0x16, 0x03, 0x01, 0xff, 0xff})
	raw("an SSH banner", []byte("SSH-2.0-OpenSSH_9.9\r\n"))
	raw("nothing but zeros", make([]byte, 100000))

	handshake := func(what string, cfg *tls.Config) {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			fail("tls %s: no connection: %v", what, err)
			return
		}
		defer func() { _ = c.Close() }()
		tc := tls.Client(c, cfg)
		_ = tc.SetDeadline(time.Now().Add(8 * time.Second))
		err = tc.Handshake()
		if err == nil {
			// The handshake may complete for a client certificate that the
			// server refuses only once it reads it (TLS 1.3): a read says.
			_, err = tc.Read(make([]byte, 1))
		}
		if err == nil {
			fail("tls %s: the server took it, and sent data", what)
			return
		}
		pass("tls %s: refused (%s)", what, short(err))
	}
	base := func() *tls.Config { return &tls.Config{InsecureSkipVerify: true} } // #nosec: an attacker checks nothing
	c := base()
	c.MaxVersion = tls.VersionTLS12
	c.NextProtos = []string{wire.ALPNJoin}
	handshake("1.2 at most, for a join", c)
	c = base()
	handshake("without a protocol (ALPN)", c)
	c = base()
	c.NextProtos = []string{"h2", "http/1.1"}
	handshake("for HTTP", c)
	c = base()
	for i := 0; i < 500; i++ {
		c.NextProtos = append(c.NextProtos, fmt.Sprintf("proto-%03d", i))
	}
	handshake("with 500 protocols, none theLB's", c)
	c = base()
	c.NextProtos = []string{wire.ALPNAPI}
	handshake("for the API, without a certificate", c)
	c = base()
	c.NextProtos = []string{wire.ALPNRaft}
	handshake("for Raft, without a certificate", c)
	c = base()
	c.NextProtos = []string{wire.ALPNAPI}
	c.Certificates = []tls.Certificate{selfSigned("director")}
	handshake("for the API, with a certificate no cluster signed", c)
	c = base()
	c.NextProtos = []string{wire.ALPNRaft}
	c.Certificates = []tls.Certificate{otherCluster("director", "lb-1")}
	handshake("for Raft, with a director's certificate from another cluster", c)
}

func short(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func selfSigned(cn string) tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// otherCluster is a certificate signed by another theLB cluster's CA - one
// named like the cluster under attack, so only the key tells them apart.
func otherCluster(role, name string) tls.Certificate {
	a, err := ca.New("thelb", time.Now())
	if err != nil {
		panic(err)
	}
	key, _ := ca.NewKey()
	_, der, err := a.Sign(role, name, []netip.Addr{netip.MustParseAddr("198.18.0.66")}, &key.PublicKey, time.Now(), time.Hour)
	if err != nil {
		panic(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func slow(addr string) {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		fail("slow: no connection: %v", err)
		return
	}
	closed, took := closedWithin(c, wire.HandshakeTimeout+3*time.Second)
	_ = c.Close()
	if !closed {
		fail("slow: a connection that never began its handshake was open after %s", took.Round(time.Millisecond))
	} else {
		pass("slow: a connection that never began its handshake closed after %s", took.Round(time.Millisecond))
	}

	// A ClientHello dripped a byte at a time: the handshake's deadline still
	// ends it.
	c, err = net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		fail("slow drip: no connection: %v", err)
		return
	}
	start := time.Now()
	hello := []byte{0x16, 0x03, 0x01, 0x02, 0x00, 0x01, 0x00, 0x01, 0xfc, 0x03, 0x03}
	done := false
	for i := 0; i < 40 && !done; i++ {
		if _, err := c.Write(hello[i%len(hello) : i%len(hello)+1]); err != nil {
			done = true
		}
		time.Sleep(300 * time.Millisecond)
	}
	_ = c.Close()
	if !done || time.Since(start) > wire.HandshakeTimeout+3*time.Second {
		fail("slow drip: a ClientHello dripped a byte every 300 ms was not cut off by %s (%s)", wire.HandshakeTimeout, time.Since(start).Round(time.Millisecond))
	} else {
		pass("slow drip: cut off after %s", time.Since(start).Round(time.Millisecond))
	}

	// A join's handshake done, then not a word: the join's own deadline.
	tc, err := joinConn(ap(addr))
	if err != nil {
		fail("slow join: the handshake failed: %v", err)
		return
	}
	closed, took = closedWithin(tc, 15*time.Second)
	_ = tc.Close()
	if !closed {
		fail("slow join: a join that said nothing after its handshake was open after %s", took.Round(time.Millisecond))
	} else {
		pass("slow join: a join that said nothing was closed after %s", took.Round(time.Millisecond))
	}
}

// ---- the join ----

func joinConn(vip netip.AddrPort) (*tls.Conn, error) {
	c, err := net.DialTimeout("tcp", vip.String(), 2*time.Second)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(c, wire.JoinConfig(vip.Addr(), nil))
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	if err := tc.Handshake(); err != nil {
		_ = c.Close()
		return nil, err
	}
	_ = tc.SetDeadline(time.Time{})
	return tc, nil
}

// hello sends a Hello and reads the Reply; closed says the server ended the
// connection without one.
func hello(vip netip.AddrPort, h func(cs tls.ConnectionState) any) (r join.Reply, closed bool, err error) {
	tc, err := joinConn(vip)
	if err != nil {
		return r, false, err
	}
	defer func() { _ = tc.Close() }()
	w := wire.NewConn(tc)
	_ = tc.SetDeadline(time.Now().Add(12 * time.Second))
	switch m := h(tc.ConnectionState()).(type) {
	case []byte:
		_, _ = tc.Write(m)
	default:
		if err := w.Send(m); err != nil {
			return r, true, nil
		}
	}
	if err := w.Recv(&r); err != nil {
		return r, true, nil
	}
	return r, false, nil
}

func ownMAC() string {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Name == "eth0" {
			return i.HardwareAddr.String()
		}
	}
	return ""
}

func ownAddr(vip netip.AddrPort) netip.Addr {
	c, err := net.Dial("udp", vip.String())
	if err != nil {
		return netip.Addr{}
	}
	defer func() { _ = c.Close() }()
	a, _ := netip.ParseAddrPort(c.LocalAddr().String())
	return a.Addr().Unmap()
}

func fingerprint() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func joins(vip netip.AddrPort, backend, director secret.Secret) {
	proof := func(s secret.Secret) func(cs tls.ConnectionState) []byte {
		return func(cs tls.ConnectionState) []byte { p, _ := wire.Proof(s, wire.SideJoiner, cs); return p }
	}
	refused := func(what string, h func(cs tls.ConnectionState) any, want string) {
		r, closed, err := hello(vip, h)
		switch {
		case err != nil:
			fail("join %s: %v", what, err)
		case r.OK:
			fail("join %s: accepted", what)
		case closed && want == "":
			pass("join %s: the connection closed, without an answer", what)
		case closed:
			fail("join %s: closed without an answer; wanted %q", what, want)
		case !strings.Contains(r.Error, want):
			fail("join %s: refused, but with %q; wanted %q", what, r.Error, want)
		case len(r.Proof) > 0:
			fail("join %s: refused, but the cluster sent its proof", what)
		default:
			pass("join %s: refused (%s)", what, r.Error)
		}
	}
	random := make([]byte, 32)
	_, _ = rand.Read(random)
	refused("with a random proof", func(tls.ConnectionState) any { return join.Hello{V: wire.Version, Role: "backend", Proof: random} }, "not accepted")
	refused("without a proof", func(tls.ConnectionState) any { return join.Hello{V: wire.Version, Role: "backend"} }, "not accepted")
	refused("with the directors' secret, as a backend", func(cs tls.ConnectionState) any {
		return join.Hello{V: wire.Version, Role: "backend", Proof: proof(director)(cs)}
	}, "not accepted")
	refused("with the backends' secret, as a director", func(cs tls.ConnectionState) any {
		return join.Hello{V: wire.Version, Role: "director", Proof: proof(backend)(cs)}
	}, "not accepted")
	refused("as a role that is none", func(cs tls.ConnectionState) any {
		return join.Hello{V: wire.Version, Role: "admin", Proof: proof(backend)(cs)}
	}, "protocol version only")
	refused("in a protocol version from the future", func(cs tls.ConnectionState) any {
		return join.Hello{V: wire.Version + 1, Role: "backend", Proof: proof(backend)(cs)}
	}, "protocol version only")
	// A proof is bound to its TLS session: one made on another connection -
	// as a man in the middle would relay it - does not prove anything.
	var other []byte
	if tc, err := joinConn(vip); err == nil {
		other = proof(backend)(tc.ConnectionState())
		_ = tc.Close()
	}
	refused("with a proof from another connection (relayed)", func(tls.ConnectionState) any {
		return join.Hello{V: wire.Version, Role: "backend", Proof: other}
	}, "not accepted")
	refused("with a message over the size limit", func(tls.ConnectionState) any {
		return append([]byte(`{"v":1,"role":"backend","proof":"`), append(make([]byte, wire.MaxLine+10), '"', '}', '\n')...)
	}, "")
	refused("with JSON that is not one", func(tls.ConnectionState) any { return []byte("{\"v\":1,\n") }, "")
	refused("with a JSON array", func(tls.ConnectionState) any { return []byte("[1,2,3]\n") }, "")

	// With the secret - an insider, or a stolen file - every lie in a claim
	// is refused, and none waits for approval.
	mac, addr := ownMAC(), ownAddr(vip)
	good := cluster.Claim{Name: "liar", Role: "backend", Address: addr.String(), MAC: mac, Fingerprint: fingerprint(), Hostname: "liar", Version: "0.1.0-attack"}
	key, _ := ca.NewKey()
	csr := func(name string) []byte { b, _ := ca.Request(key, name); return b }
	lie := func(what string, mutate func(*cluster.Claim, *[]byte), want string) {
		c := good
		req := csr(c.Name)
		mutate(&c, &req)
		tc, err := joinConn(vip)
		if err != nil {
			fail("claim %s: %v", what, err)
			return
		}
		defer func() { _ = tc.Close() }()
		w := wire.NewConn(tc)
		_ = tc.SetDeadline(time.Now().Add(12 * time.Second))
		if w.Send(join.Hello{V: wire.Version, Role: "backend", Proof: proof(backend)(tc.ConnectionState())}) != nil {
			fail("claim %s: the hello was not sent", what)
			return
		}
		var r join.Reply
		if w.Recv(&r) != nil || !r.OK {
			fail("claim %s: the secret was not accepted: %+v", what, r)
			return
		}
		if w.Send(join.Request{Claim: c, CSR: req}) != nil {
			fail("claim %s: the request was not sent", what)
			return
		}
		var st join.State
		if err := w.Recv(&st); err != nil {
			fail("claim %s: no answer: %v", what, err)
			return
		}
		if st.State != join.Refused || !strings.Contains(st.Message, want) {
			fail("claim %s: %s %q; wanted refused, saying %q", what, st.State, st.Message, want)
			return
		}
		pass("claim %s: refused (%s)", what, st.Message)
	}
	lie("named in capitals", func(c *cluster.Claim, r *[]byte) { c.Name = "LIAR"; *r = csr(c.Name) }, "is not a node's name")
	lie("with a name of 64 characters", func(c *cluster.Claim, r *[]byte) { c.Name = strings.Repeat("a", 64); *r = csr(c.Name) }, "is not a node's name")
	lie("with a newline in its name", func(c *cluster.Claim, r *[]byte) { c.Name = "li\nar"; *r = csr(c.Name) }, "is not a node's name")
	lie("as a director, with the backends' secret", func(c *cluster.Claim, _ *[]byte) { c.Role = "director" }, "cannot join a director")
	lie("from an address it does not have", func(c *cluster.Claim, _ *[]byte) { c.Address = "198.18.0.21" }, "not from the address claimed")
	lie("with a MAC address not its own", func(c *cluster.Claim, _ *[]byte) { c.MAC = "02:00:00:00:00:99" }, "is not the one claimed")
	lie("with a MAC address that is none", func(c *cluster.Claim, _ *[]byte) { c.MAC = "02-00-00-00-00-99" }, "MAC address claimed is not one")
	lie("with a fingerprint that is none", func(c *cluster.Claim, _ *[]byte) { c.Fingerprint = "../../etc/passwd" }, "node fingerprint is not one")
	lie("with a hostname holding a terminal's escape", func(c *cluster.Claim, _ *[]byte) { c.Hostname = "x\x1b[2J" }, "hostname claimed is not")
	lie("with a version holding a newline", func(c *cluster.Claim, _ *[]byte) { c.Version = "1\nWARN forged" }, "version claimed is not")
	lie("with a request for another node's name", func(c *cluster.Claim, r *[]byte) { *r = csr("lb-1") }, "not for its name")
	lie("with a request whose signature does not check", func(c *cluster.Claim, r *[]byte) {
		b := csr(c.Name)
		b[len(b)-3] ^= 0xff
		*r = b
	}, "not for its name")
	lie("with a request for an RSA key", func(c *cluster.Claim, r *[]byte) {
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		b, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: c.Name}}, k)
		*r = b
	}, "not for its name")
	lie("with no request at all", func(_ *cluster.Claim, r *[]byte) { *r = nil }, "not for its name")
}

func joinflood(vip netip.AddrPort, n int) {
	var wg sync.WaitGroup
	var refusedN, closedN, errN atomic.Int64
	start := time.Now()
	sem := make(chan struct{}, 64)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			bad := make([]byte, 32)
			_, _ = rand.Read(bad)
			r, closed, err := hello(vip, func(tls.ConnectionState) any { return join.Hello{V: wire.Version, Role: "backend", Proof: bad} })
			switch {
			case err != nil:
				errN.Add(1)
			case closed:
				closedN.Add(1)
			case !r.OK:
				refusedN.Add(1)
			default:
				fail("joinflood: a wrong secret was accepted")
			}
		}()
	}
	wg.Wait()
	info("joinflood: %d joins with a wrong secret in %s: %d refused, %d closed, %d failed to connect", n, time.Since(start).Round(time.Millisecond), refusedN.Load(), closedN.Load(), errN.Load())
	if refusedN.Load()+closedN.Load()+errN.Load() == int64(n) {
		pass("joinflood: none of %d accepted", n)
	}
}

// pendingFlood asks to join under n new names, with the backends' secret and
// claims that are all true - but for the name - and leaves each waiting. How
// many the cluster records, the lab counts.
func pendingFlood(vip netip.AddrPort, s secret.Secret, n int) {
	mac, addr := ownMAC(), ownAddr(vip)
	var waiting atomic.Int64
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sem := make(chan struct{}, 32)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			name := fmt.Sprintf("flood-%04d", i)
			key, _ := ca.NewKey()
			csr, _ := ca.Request(key, name)
			once := sync.Once{}
			a := join.Asker{VIP: vip, Role: "backend", Secret: s, Claim: cluster.Claim{Name: name, Role: "backend", Address: addr.String(), MAC: mac, Fingerprint: fingerprint(), Hostname: name, Version: "0.1.0-attack"},
				CSR: csr, Dial: func(ctx context.Context) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", vip.String())
				}}
			jctx, jcancel := context.WithCancel(ctx)
			defer jcancel()
			a.Waiting = func() { once.Do(func() { waiting.Add(1); jcancel() }) }
			_, _ = a.Ask(jctx)
		}(i)
	}
	wg.Wait()
	info("pending: %d of %d new names were told to wait for approval", waiting.Load(), n)
}

// ---- a member gone bad ----

type identity struct {
	Name   string `json:"name"`
	Member string `json:"member"`
	Key    []byte `json:"key"`
	Cert   []byte `json:"cert"`
	CA     []byte `json:"ca"`
}

func (id identity) wire() (*wire.Identity, *ecdsa.PrivateKey, error) {
	caCert, err := x509.ParseCertificate(id.CA)
	if err != nil {
		return nil, nil, err
	}
	k, err := x509.ParseECPrivateKey(id.Key)
	if err != nil {
		return nil, nil, err
	}
	w, err := wire.NewIdentity(caCert)
	if err != nil {
		return nil, nil, err
	}
	if err := w.SetCertificate(k, id.Cert); err != nil {
		return nil, nil, err
	}
	return w, k, nil
}

// call sends one API request as id, and reads one answer.
func call(vip netip.AddrPort, wid *wire.Identity, proto string, req any) (api.Response, error) {
	c, err := net.DialTimeout("tcp", vip.String(), 2*time.Second)
	if err != nil {
		return api.Response{}, err
	}
	defer func() { _ = c.Close() }()
	tc := tls.Client(c, wid.ClientConfig(proto, vip.Addr()))
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.Handshake(); err != nil {
		return api.Response{}, fmt.Errorf("handshake: %w", err)
	}
	w := wire.NewConn(tc)
	if err := w.Send(req); err != nil {
		return api.Response{}, err
	}
	var resp api.Response
	if err := w.Recv(&resp); err != nil {
		return api.Response{}, fmt.Errorf("no answer: %w", err)
	}
	return resp, nil
}

func insider(vip netip.AddrPort, s secret.Secret, name, dir string, node netip.AddrPort) {
	key, _ := ca.NewKey()
	csr, _ := ca.Request(key, name)
	mac, addr := ownMAC(), ownAddr(vip)
	a := join.Asker{VIP: vip, Role: "backend", Secret: s, Claim: cluster.Claim{Name: name, Role: "backend", Address: addr.String(), MAC: mac, Fingerprint: fingerprint(), Hostname: name, Version: "0.1.0-attack"},
		CSR: csr, Dial: func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", vip.String())
		}}
	waitingSaid := false
	a.Waiting = func() {
		if !waitingSaid {
			waitingSaid = true
			fmt.Println("WAITING for approval")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	approval, err := a.Ask(ctx)
	if err != nil {
		fail("insider: not approved: %v", err)
		return
	}
	member := name + "-" + fingerprint()[:8]
	if _, err := approval.Ready(member, ""); err != nil {
		fail("insider: not added: %v", err)
		return
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	id := identity{Name: name, Member: member, Key: kb, Cert: approval.Cert, CA: approval.CA.Raw}
	b, _ := json.Marshal(id)
	if err := os.WriteFile(filepath.Join(dir, "insider.json"), b, 0o600); err != nil {
		fail("insider: %v", err)
		return
	}
	pass("insider: joined as the backend %s, approved", name)
	wid, _, err := id.wire()
	if err != nil {
		fail("insider: its identity: %v", err)
		return
	}
	v := wire.Version
	expect := func(what string, req api.Request, want string) {
		resp, err := call(vip, wid, wire.ALPNAPI, req)
		switch {
		case err != nil:
			fail("insider %s: %v", what, err)
		case resp.OK:
			fail("insider %s: the leader did it", what)
		case !strings.Contains(resp.Error, want):
			fail("insider %s: refused with %q; wanted %q", what, resp.Error, want)
		default:
			pass("insider %s: refused (%s)", what, resp.Error)
		}
	}
	// A backend may heartbeat, offer, and renew its own certificate -
	// nothing a director may.
	expect("adds a pool", api.Request{V: v, Op: api.OpChange, Change: &api.Change{Op: "pool.add", Pool: "evil", VIP: "198.18.0.200/24", Count: 4}}, "a backend may not change")
	expect("approves itself a friend", api.Request{V: v, Op: api.OpChange, Change: &api.Change{Op: "approve", Names: []string{"flood-0001"}}}, "a backend may not change")
	expect("removes a director", api.Request{V: v, Op: api.OpChange, Change: &api.Change{Op: "remove", Names: []string{"d2"}}}, "a backend may not change")
	expect("asks what only directors see", api.Request{V: v, Op: api.OpLive}, "a backend may not live")
	expect("takes a director out of the vote", api.Request{V: v, Op: api.OpLeave, RaftID: "d2-00000000"}, "a backend may not leave")
	expect("checks in as a director", api.Request{V: v, Op: api.OpCheckIn, RaftID: "d2-00000000"}, "a backend may not checkin")
	expect("speaks a protocol version from the future", api.Request{V: v + 1, Op: api.OpBackend}, "version")
	expect("asks an operation that is none", api.Request{V: v, Op: "shutdown"}, "a backend may not shutdown")
	expect("offers on port 0", api.Request{V: v, Op: api.OpOffer, Offer: &api.Offer{Op: "add", Offer: cluster.Offer{Name: "x", Proto: "tcp", Port: 0}}}, "")
	expect("offers under a name with a newline", api.Request{V: v, Op: api.OpOffer, Offer: &api.Offer{Op: "add", Offer: cluster.Offer{Name: "a\nb", Proto: "tcp", Port: 80}}}, "")
	expect("offers with a check holding an escape", api.Request{V: v, Op: api.OpOffer, Offer: &api.Offer{Op: "add", Offer: cluster.Offer{Name: "x", Proto: "tcp", Port: 80, Check: "http:/\x1b[2J"}}}, "")
	expect("offers over icmp", api.Request{V: v, Op: api.OpOffer, Offer: &api.Offer{Op: "add", Offer: cluster.Offer{Name: "x", Proto: "icmp", Port: 80}}}, "")
	expect("offers with no offer", api.Request{V: v, Op: api.OpOffer}, "")
	holding := make([]string, 4097)
	for i := range holding {
		holding[i] = "198.18.0.50"
	}
	expect("sends a heartbeat holding 4097 VIPs", api.Request{V: v, Op: api.OpBackend, Beat: &api.Beat{Member: member, Version: "0.1.0-attack", Holding: holding}}, "at most 4096")
	checks := map[string]bool{}
	for i := 0; i < 5000; i++ {
		checks[fmt.Sprintf("tcp/%d", i)] = true
	}
	expect("sends a heartbeat with 5000 checks", api.Request{V: v, Op: api.OpBackend, Beat: &api.Beat{Member: member, Version: "0.1.0-attack", Checks: checks}}, "at most 4096")

	// Renewing: whatever name its request asks for, the leader signs its
	// own, as a backend.
	k2, _ := ca.NewKey()
	other, _ := ca.Request(k2, "d1")
	resp, err := call(vip, wid, wire.ALPNAPI, api.Request{V: v, Op: api.OpRenew, CSR: other})
	switch {
	case err != nil:
		fail("insider renews as d1: %v", err)
	case !resp.OK:
		pass("insider renews as d1: refused (%s)", resp.Error)
	default:
		cert, err := x509.ParseCertificate(resp.Cert)
		if err != nil {
			fail("insider renews as d1: the certificate does not read: %v", err)
			break
		}
		peer, ok := ca.PeerOf(cert)
		if !ok || peer.Name != name || peer.Role != "backend" {
			fail("insider renews as d1: signed as %+v", peer)
		} else {
			pass("insider renews as d1: signed for itself only (%s, a backend)", peer.Name)
		}
	}

	// Its certificate, on the directors' own listener: Raft, and the API
	// there, are for directors.
	for _, proto := range []string{wire.ALPNRaft, wire.ALPNAPI} {
		c, err := net.DialTimeout("tcp", node.String(), 2*time.Second)
		if err != nil {
			fail("insider on %s at %s: %v", proto, node, err)
			continue
		}
		cfg := wid.ClientConfig(proto, node.Addr())
		cfg.InsecureSkipVerify = true // it would refuse the server's name; the server must refuse the client
		cfg.VerifyConnection = nil
		tc := tls.Client(c, cfg)
		_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
		err = tc.Handshake()
		if err == nil {
			_, err = tc.Read(make([]byte, 1))
		}
		_ = c.Close()
		if err == nil {
			fail("insider on %s at %s: the director took a backend's certificate", proto, node)
		} else {
			pass("insider on %s at %s: refused (%s)", proto, node, short(err))
		}
	}
}

func revoked(vip netip.AddrPort, dir string) {
	b, err := os.ReadFile(filepath.Join(dir, "insider.json"))
	if err != nil {
		fail("revoked: %v", err)
		return
	}
	var id identity
	_ = json.Unmarshal(b, &id)
	wid, _, err := id.wire()
	if err != nil {
		fail("revoked: %v", err)
		return
	}
	resp, err := call(vip, wid, wire.ALPNAPI, api.Request{V: wire.Version, Op: api.OpBackend, Beat: &api.Beat{Member: id.Member, Version: "0.1.0-attack"}})
	switch {
	case err != nil:
		pass("revoked: the removed backend's certificate is refused (%s)", short(err))
	case resp.OK && resp.Orders != nil && resp.Orders.Removed:
		pass("revoked: the removed backend is told it was removed, and nothing else")
	default:
		fail("revoked: the removed backend was answered: %+v", resp)
	}
}

// ---- the metrics endpoint ----

func metrics(addr string) {
	get := func(what, req string, want string) {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			fail("metrics %s: %v", what, err)
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		_, _ = io.WriteString(c, req)
		line, _ := bufio.NewReader(c).ReadString('\n')
		if !strings.Contains(line, want) {
			fail("metrics %s: %q; wanted %q", what, strings.TrimSpace(line), want)
			return
		}
		pass("metrics %s: %s", what, strings.TrimSpace(line))
	}
	get("GET /metrics", "GET /metrics HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", "200")
	get("POST /metrics", "POST /metrics HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", "405")
	get("a path out of the tree", "GET /../../etc/passwd HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", "")
	get("a path that is none", "GET /debug/pprof/ HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", "404")
	get("TRACE", "TRACE /metrics HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", "405")
	get("headers over a megabyte", "GET /metrics HTTP/1.1\r\nHost: x\r\nX: "+strings.Repeat("a", 2<<20)+"\r\n\r\n", "431")
	get("a request line that is none", "\x16\x03\x01\x00\x10garbage\r\n\r\n", "400")

	// Headers dripped a byte a second (slowloris): cut off by the read
	// header timeout.
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		fail("metrics slowloris: %v", err)
		return
	}
	start := time.Now()
	_, _ = io.WriteString(c, "GET /metrics HTTP/1.1\r\nHost: x\r\n")
	cut := false
	for i := 0; i < 20 && !cut; i++ {
		if _, err := io.WriteString(c, "X"); err != nil {
			cut = true
		}
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		if n, err := c.Read(make([]byte, 512)); n > 0 || (err != nil && !isTimeout(err)) {
			cut = true
		}
	}
	_ = c.Close()
	if !cut {
		fail("metrics slowloris: headers dripped for %s were never cut off", time.Since(start).Round(time.Second))
	} else {
		pass("metrics slowloris: cut off after %s", time.Since(start).Round(time.Second))
	}

	// Many at once, each holding its connection open.
	var conns []net.Conn
	for i := 0; i < 500; i++ {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			break
		}
		conns = append(conns, c)
	}
	info("metrics: %d connections held open at once", len(conns))
	get("GET /metrics beside them", "GET /metrics HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", "200")
	for _, c := range conns {
		_ = c.Close()
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// ---- a Kubernetes API server that lies ----

// evilkube serves a Kubernetes API that answers like one, then misbehaves as
// mode says, on TLS with a certificate of its own; it writes a kubeconfig for
// it in dir, with a token the lab looks for wherever it should not be.
func evilkube(listen, mode, dir string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	host, _, _ := net.SplitHostPort(listen)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "evil"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP(host)}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kc := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
  - name: evil
    cluster:
      server: https://%s
      certificate-authority-data: %s
users:
  - name: u
    user:
      token: evil-token-not-to-be-seen
contexts:
  - name: evil
    context:
      cluster: evil
      user: u
current-context: evil
`, listen, b64(caPEM))
	if err := os.WriteFile(filepath.Join(dir, "kubeconfig"), []byte(kc), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ln, err := tls.Listen("tcp", listen, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// A second listener, in clear, where a redirect points: anything that
	// arrives there - with a token - the lab sees in seen.log.
	seen, _ := os.OpenFile(filepath.Join(dir, "seen.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	trap, err := net.Listen("tcp", net.JoinHostPort(host, "18080"))
	if err == nil {
		go func() {
			for {
				c, err := trap.Accept()
				if err != nil {
					return
				}
				go func() {
					defer func() { _ = c.Close() }()
					_ = c.SetDeadline(time.Now().Add(5 * time.Second))
					b := make([]byte, 8192)
					n, _ := c.Read(b)
					_, _ = seen.Write(append(b[:n], '\n'))
					_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}")
				}()
			}
		}()
	}
	fmt.Println("READY")
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go serveEvil(c, mode, host)
	}
}

func b64(b []byte) string {
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var sb strings.Builder
	for i := 0; i < len(b); i += 3 {
		var v uint32
		n := min(3, len(b)-i)
		for j := 0; j < 3; j++ {
			v <<= 8
			if j < n {
				v |= uint32(b[i+j])
			}
		}
		for j := 0; j < 4; j++ {
			if j <= n {
				sb.WriteByte(enc[(v>>(18-6*j))&63])
			} else {
				sb.WriteByte('=')
			}
		}
	}
	return sb.String()
}

func serveEvil(c net.Conn, mode, host string) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	for {
		_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return
		}
		path := parts[1]
		for {
			h, err := r.ReadString('\n')
			if err != nil || h == "\r\n" {
				break
			}
		}
		write := func(code int, body string) {
			_, _ = fmt.Fprintf(c, "HTTP/1.1 %d X\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", code, len(body), body)
		}
		switch {
		case strings.HasPrefix(path, "/version"):
			write(200, `{"major":"1","minor":"36","gitVersion":"v1.36.0-evil"}`)
			continue
		case strings.Contains(path, "selfsubjectaccessreviews"):
			write(201, `{"status":{"allowed":true}}`)
			continue
		}
		watch := strings.Contains(path, "watch=1")
		switch mode {
		case "redirect":
			_, _ = fmt.Fprintf(c, "HTTP/1.1 302 Found\r\nLocation: http://%s:18080%s\r\nContent-Length: 0\r\n\r\n", host, path)
		case "status500":
			write(500, `{"kind":"Status","code":500,"message":"etcd\nis down\u001b[2J"}`)
		case "badjson":
			write(200, `{"metadata":{"resourceVersion":"1"},"items":[{"metadata":`)
		case "hugelist":
			// An endless list: theLB must not hold it all.
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"metadata\":{\"resourceVersion\":\"1\"},\"items\":[")
			item := `{"metadata":{"name":"n","labels":{"x":"` + strings.Repeat("y", 60) + `"}}},`
			chunk := strings.Repeat(item, 1000)
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := io.WriteString(c, chunk); err != nil {
					return
				}
			}
			return
		case "giantline":
			if !watch {
				write(200, `{"metadata":{"resourceVersion":"1"},"items":[]}`)
				continue
			}
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"type\":\"ADDED\",\"object\":{\"metadata\":{\"name\":\"")
			chunk := strings.Repeat("a", 1<<20)
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := io.WriteString(c, chunk); err != nil {
					return
				}
			}
			return
		case "drip":
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n")
			for _, b := range []byte(`{"metadata":{"resourceVersion":"1"},"items":[]}`) {
				if _, err := c.Write([]byte{b}); err != nil {
					return
				}
				time.Sleep(time.Second)
			}
			return
		default:
			write(200, `{"metadata":{"resourceVersion":"1"},"items":[]}`)
		}
	}
}

// ---- a cluster that is not one ----

// impostor answers on listen as a theLB cluster would - a chain from a theLB
// CA of its own, for its address, a director's certificate - takes a join's
// hello, and answers it with a proof it cannot make. A node must stop there:
// it never sends its claim or its certificate request to what did not prove
// the secret. What arrives after the hello, impostor writes to dir/after-hello.
func impostor(listen netip.AddrPort, dir string) {
	a, err := ca.New("thelb", time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	leaf, err := a.Issue("director", "lb-1", []netip.Addr{listen.Addr()}, time.Now(), time.Hour)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cert := tls.Certificate{Certificate: [][]byte{leaf.Cert.Raw, a.DER}, PrivateKey: leaf.Key}
	ln, err := tls.Listen("tcp", listen.String(), &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{wire.ALPNJoin}, MinVersion: tls.VersionTLS13})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	after, _ := os.OpenFile(filepath.Join(dir, "after-hello"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	fmt.Println("READY")
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = c.Close() }()
			w := wire.NewConn(c)
			_ = c.SetDeadline(time.Now().Add(20 * time.Second))
			var h join.Hello
			if w.Recv(&h) != nil {
				return
			}
			fake := make([]byte, 32)
			_, _ = rand.Read(fake)
			_ = w.Send(join.Reply{V: wire.Version, OK: true, Proof: fake, ClusterID: "impostor"})
			b := make([]byte, 65536)
			n, _ := c.Read(b)
			if n > 0 {
				_, _ = after.Write(append(b[:n], '\n'))
			}
		}()
	}
}
