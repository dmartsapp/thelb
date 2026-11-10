package director

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/fingerprint"
	"github.com/dmartsapp/thelb/internal/hostnet"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/secret"
	"github.com/dmartsapp/thelb/internal/status"
	"github.com/dmartsapp/thelb/internal/wire"
)

func TestMain(m *testing.M) {
	joinRetry, checkInEvery, liveFor, sweepEvery = 200*time.Millisecond, 200*time.Millisecond, time.Second, time.Second
	secret.CheckOwner = func(os.FileInfo) error { return nil } // the tests' files are not root's
	os.Exit(m.Run())
}

func TestDecideIsTheDesignsTable(t *testing.T) {
	cases := []struct {
		exists, bootstrap bool
		p                 probe
		want              action
	}{
		{true, true, probeTheLB, actJoin},
		{true, false, probeTheLB, actJoin},
		{false, true, probeTheLB, actStopNoSecret},
		{false, false, probeTheLB, actStopNoSecret},
		{true, true, probeNothing, actBootstrap},
		{false, true, probeNothing, actCreate},
		{true, false, probeNothing, actWait},
		{false, false, probeNothing, actStopNoSecret},
		{true, true, probeOther, actRefuseTaken},
		{false, false, probeOther, actRefuseTaken},
	}
	for _, c := range cases {
		if got := decide(c.exists, c.bootstrap, c.p); got != c.want {
			t.Errorf("decide(exists=%v, bootstrap=%v, probe=%v) = %v, want %v", c.exists, c.bootstrap, c.p, got, c.want)
		}
	}
}

// syncBuffer is a log the director writes from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// loopNet is a director's network on the loopback interface: the control VIP
// is an address and port that whoever leads listens on.
type loopNet struct {
	node  string // where this node listens: "127.0.0.1:0", or "127.0.0.11:<port>"
	local string // where its connections come from; "" for anywhere
	vip   string
	macs  *sync.Map // the neighbour table: address -> MAC address
	held  string    // the MAC address VIPHolder finds holding the VIP; "" for none

	mu    sync.Mutex
	ln    net.Listener
	takes int
}

func (l *loopNet) ListenNode() (net.Listener, error) { return net.Listen("tcp", l.node) }

func (l *loopNet) dial(ctx context.Context, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: time.Second}
	if l.local != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(l.local)}
	}
	return d.DialContext(ctx, "tcp", addr)
}

func (l *loopNet) DialVIP(ctx context.Context) (net.Conn, error)           { return l.dial(ctx, l.vip) }
func (l *loopNet) Dial(ctx context.Context, addr string) (net.Conn, error) { return l.dial(ctx, addr) }
func (l *loopNet) VIPHolder(context.Context) (string, error)               { return l.held, nil }
func (l *loopNet) Holder(context.Context, netip.Addr) (string, error)      { return "", nil }
func (l *loopNet) Uplink(context.Context) (bool, string)                   { return true, "" }
func (l *loopNet) Cleanup() error                                          { return nil }
func (l *loopNet) Repair() (bool, error)                                   { return false, nil }

func (l *loopNet) MACOf(ip netip.Addr) string {
	if v, ok := l.macs.Load(ip.String()); ok {
		return v.(string)
	}
	return ""
}

func (l *loopNet) Take() (net.Listener, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ln != nil {
		return l.ln, nil
	}
	// In one process, the director that led may not have let go yet.
	var err error
	for range 40 {
		var ln net.Listener
		if ln, err = net.Listen("tcp", l.vip); err == nil {
			l.ln = ln
			l.takes++
			return ln, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, err
}

func (l *loopNet) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ln != nil {
		_ = l.ln.Close()
		l.ln = nil
	}
}

func (l *loopNet) holding() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ln != nil
}

func freePort(t *testing.T, ip string) int {
	t.Helper()
	ln, err := net.Listen("tcp", ip+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// world is one director under test.
type world struct {
	dir     string
	run     string
	cfg     Config
	local   hostnet.Address
	net     *loopNet
	machine string
	plane   Dataplane // nil: none
	logs    *syncBuffer
	started chan struct{}
	stop    context.CancelFunc
	errc    chan error
}

// newWorld is a director whose node address is addr, listening on node, with
// the control VIP at vip (an address and port on the loopback interface).
func newWorld(t *testing.T, name string, addr netip.Prefix, mac, node, local, vip string, macs *sync.Map) *world {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "thelb-dir-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	macs.Store(addr.Addr().String(), mac)
	return &world{
		dir: base,
		run: filepath.Join(base, "run"),
		cfg: Config{
			ControlVIP: netip.MustParseAddr("10.0.0.10"), ControlPort: 7443,
			SecretFile: filepath.Join(base, "director.secret"), BackendSecretFile: filepath.Join(base, "backend.secret"),
			Bootstrap: true, Cluster: "prod", Name: name,
		},
		local:   hostnet.Address{Interface: "eth1", MAC: mac, Prefix: addr},
		net:     &loopNet{node: node, local: local, vip: vip, macs: macs},
		machine: fmt.Sprintf("4c2f3a0e9b7d4e1fa6c58d2b91e0f7%02x", len(name)*7+int(name[len(name)-1])),
		logs:    &syncBuffer{},
	}
}

// single is a director alone, on any platform: its node on 127.0.0.1, the VIP
// on another port there.
func single(t *testing.T) *world {
	t.Helper()
	vip := fmt.Sprintf("127.0.0.1:%d", freePort(t, "127.0.0.1"))
	return newWorld(t, "lb-1", netip.MustParsePrefix("10.0.0.11/24"), "52:54:00:00:00:11", "127.0.0.1:0", "", vip, &sync.Map{})
}

func (w *world) deps() Deps {
	w.started = make(chan struct{})
	id, _ := fingerprint.ParseMachineID(w.machine)
	started := w.started
	return Deps{
		RunDir: w.run,
		Log:    logx.New(w.logs, "thelb"),
		Now:    time.Now,
		Host: func(mac string) (fingerprint.Inputs, error) {
			return fingerprint.Inputs{MachineID: id, MAC: mac, Hostname: w.cfg.Name}, nil
		},
		Network:     func(hostnet.Address) (Network, error) { return w.net, nil },
		Local:       func(netip.Addr, string) (hostnet.Address, error) { return w.local, nil },
		PeerUID:     func(*net.UnixConn) (int, error) { return 0, nil },
		Ready:       func() { close(started) },
		ProbeWindow: 300 * time.Millisecond,
		Dataplane: func() (Dataplane, error) {
			if w.plane == nil {
				return nothing{}, nil
			}
			return w.plane, nil
		},
	}
}

// launch runs the director until the test ends, or until stopped.
func (w *world) launch(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w.stop, w.errc = cancel, make(chan error, 1)
	d := w.deps() // before the goroutine: it makes the channel waited on
	errc := w.errc
	go func() { errc <- Run(ctx, w.cfg, d) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			_ = err // how it ended is each test's to check
		case <-time.After(30 * time.Second):
			t.Errorf("%s did not stop", w.cfg.Name)
		}
	})
}

// start launches the director and waits until it is a member.
func (w *world) start(t *testing.T) {
	t.Helper()
	w.launch(t)
	w.waitReady(t)
}

func (w *world) waitReady(t *testing.T) {
	t.Helper()
	select {
	case <-w.started:
	case err := <-w.errc:
		t.Fatalf("%s stopped: %v\n%s", w.cfg.Name, err, w.logs)
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not become a member\n%s", w.cfg.Name, w.logs)
	}
}

// halt stops the director and returns how it ended.
func (w *world) halt(t *testing.T) error {
	t.Helper()
	w.stop()
	select {
	case err := <-w.errc:
		w.errc <- err // for the cleanup
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not stop\n%s", w.cfg.Name, w.logs)
		return nil
	}
}

func (w *world) status(t *testing.T) status.Status {
	t.Helper()
	resp, err := localsock.Call(w.run, localsock.Request{Op: localsock.OpStatus})
	if err != nil {
		t.Fatalf("%s status: %v", w.cfg.Name, err)
	}
	return *resp.Status
}

// eventually waits for cond, for up to 20 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestBootstrapCreatesTheSecretsAndACluster(t *testing.T) {
	w := single(t)
	w.start(t)

	for _, f := range []string{w.cfg.SecretFile, w.cfg.BackendSecretFile} {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %04o, want 0600", f, fi.Mode().Perm())
		}
		if _, err := secret.Load(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}

	eventually(t, "the leader to be recorded", func() bool { return !w.status(t).Cluster.LeaderSince.IsZero() })
	st := w.status(t)
	if c := st.Cluster; c.Name != "prod" || len(c.ID) != 8 || c.Leader != "lb-1" || c.Term == nil || *c.Term < 1 {
		t.Errorf("cluster %+v", c)
	}
	if len(st.Nodes) != 1 || st.Nodes[0].Role != "leader" || st.Nodes[0].State != "up" || st.Nodes[0].Address != "10.0.0.11" || len(st.Nodes[0].NodeFingerprint) != 16 {
		t.Errorf("nodes %+v", st.Nodes)
	}
	if len(st.Certificates) != 2 || st.Certificates[0].Kind != "ca" || st.Certificates[1].Certificate != "lb-1" {
		t.Errorf("certificates %+v", st.Certificates)
	}
	if len(st.VIPs) != 1 || st.VIPs[0].Purpose != "control" || st.VIPs[0].HeldBy != "lb-1" || st.VIPs[0].VIP != "10.0.0.10" {
		t.Errorf("vips %+v", st.VIPs)
	}

	// The control VIP answers a join's handshake, as this cluster's leader.
	var cl wire.Cluster
	c, err := tls.Dial("tcp", w.net.vip, wire.JoinConfig(w.cfg.ControlVIP, &cl))
	if err != nil {
		t.Fatalf("the control VIP: %v", err)
	}
	_ = c.Close()
	if cl.Name != "prod" || cl.Leader != "lb-1" {
		t.Errorf("the control VIP answered as %+v", cl)
	}

	for role, file := range map[string]string{"director": w.cfg.SecretFile, "backend": w.cfg.BackendSecretFile} {
		resp, err := localsock.Call(w.run, localsock.Request{Op: localsock.OpSecretShow, Role: role})
		if err != nil {
			t.Fatal(err)
		}
		want, _ := secret.Load(file)
		if resp.Secret != want.Encode() {
			t.Errorf("secret show --role %s is not the file's secret", role)
		}
	}

	for _, line := range []string{"OK secret file created role=director", "OK secret file created role=backend", "OK cluster created cluster=prod", "OK certificate authority created", "OK leader node=lb-1 term=", "OK control VIP taken vip=10.0.0.10", "OK director ready"} {
		if !strings.Contains(w.logs.String(), line) {
			t.Errorf("the log lacks %q:\n%s", line, w.logs)
		}
	}
}

func TestNoSecretEverReachesTheLog(t *testing.T) {
	w := single(t)
	w.start(t)
	for _, f := range []string{w.cfg.SecretFile, w.cfg.BackendSecretFile} {
		s, err := secret.Load(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := localsock.Call(w.run, localsock.Request{Op: localsock.OpSecretShow, Role: "director"}); err != nil {
			t.Fatal(err)
		}
		raw := strings.TrimPrefix(s.Encode(), "thelb1:")
		if strings.Contains(w.logs.String(), raw) {
			t.Errorf("a secret reached the log:\n%s", w.logs)
		}
	}
}

func TestExistingSecretFilesAreNeverRewritten(t *testing.T) {
	w := single(t)
	s, _ := secret.Generate()
	b, _ := secret.Generate()
	if err := secret.CreateExclusive(w.cfg.SecretFile, s); err != nil {
		t.Fatal(err)
	}
	if err := secret.CreateExclusive(w.cfg.BackendSecretFile, b); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(w.cfg.SecretFile)
	w.start(t)
	after, _ := os.ReadFile(w.cfg.SecretFile)
	if string(before) != string(after) {
		t.Error("the directors' secret file changed")
	}
	if strings.Contains(w.logs.String(), "secret file created") {
		t.Errorf("a file was created:\n%s", w.logs)
	}
}

func TestOneDirectorPerNode(t *testing.T) {
	w := single(t)
	w.start(t)
	err := Run(context.Background(), w.cfg, w.deps())
	if !errors.Is(err, localsock.ErrAlreadyRunning) {
		t.Errorf("a second director: %v, want ErrAlreadyRunning", err)
	}
}

func TestWithoutBootstrapADirectorWaitsForACluster(t *testing.T) {
	w := single(t)
	w.cfg.Bootstrap = false
	err := Run(context.Background(), w.cfg, w.deps())
	if err == nil || !strings.Contains(err.Error(), "no secret in") {
		t.Errorf("no file, no --bootstrap: %v", err)
	}
	if _, statErr := os.Stat(w.cfg.SecretFile); statErr == nil {
		t.Error("a secret file was created without --bootstrap")
	}
	s, _ := secret.Generate()
	if err := secret.CreateExclusive(w.cfg.SecretFile, s); err != nil {
		t.Fatal(err)
	}
	b, _ := secret.Generate()
	if err := secret.CreateExclusive(w.cfg.BackendSecretFile, b); err != nil {
		t.Fatal(err)
	}
	w.launch(t)
	eventually(t, "the director to say it waits", func() bool {
		return strings.Contains(w.logs.String(), "WARN no cluster answers on the control VIP yet; waiting")
	})
	if st := w.status(t); st.Cluster != nil || st.Nodes[0].State != "looking for the cluster on the control VIP" {
		t.Errorf("a waiting director's status: %+v %+v", st.Cluster, st.Nodes)
	}
	if err := w.halt(t); err != nil {
		t.Errorf("stopping a waiting director: %v", err)
	}
	if _, err := os.Stat(w.run); !os.IsNotExist(err) {
		t.Errorf("the run directory was left behind: %v", err)
	}
	if strings.Contains(w.logs.String(), "cluster created") {
		t.Error("a director without --bootstrap created a cluster")
	}
}

func TestStoppingReleasesTheVIPAndLeavesNoSocket(t *testing.T) {
	w := single(t)
	w.start(t)
	eventually(t, "the leader to hold the control VIP", w.net.holding)
	if err := w.halt(t); err != nil {
		t.Fatal(err)
	}
	if w.net.holding() {
		t.Error("the control VIP is still held")
	}
	if _, err := os.Stat(w.run); !os.IsNotExist(err) {
		t.Errorf("the run directory is still there: %v", err)
	}
	for _, line := range []string{"OK control VIP released", "OK stopped"} {
		if !strings.Contains(w.logs.String(), line) {
			t.Errorf("the log lacks %q:\n%s", line, w.logs)
		}
	}
}

func TestSomethingElseOnTheControlVIPIsRefused(t *testing.T) {
	w := single(t)
	ln, err := net.Listen("tcp", w.net.vip)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.9\r\n"))
			_ = c.Close()
		}
	}()
	err = Run(context.Background(), w.cfg, w.deps())
	if err == nil || !strings.Contains(err.Error(), "the control VIP or port is taken") {
		t.Errorf("something else on the VIP: %v", err)
	}
	if _, statErr := os.Stat(w.cfg.SecretFile); statErr == nil {
		t.Error("a secret file was created for a VIP that is taken")
	}
}

// TestAHeldVIPWhosePortDropsConnectionsSaysSo: a machine answers ARP for the
// VIP, but no connection reaches the port - a firewall on theLB's leader, often
// - and the error says that, not merely that the VIP is taken.
func TestAHeldVIPWhosePortDropsConnectionsSaysSo(t *testing.T) {
	w := single(t)
	w.net.held = "02:11:32:24:ac:11"
	err := Run(context.Background(), w.cfg, w.deps())
	if err == nil || !strings.Contains(err.Error(), "02:11:32:24:ac:11 holds ") ||
		!strings.Contains(err.Error(), "but nothing accepts a connection on port") || !strings.Contains(err.Error(), "firewall") {
		t.Errorf("a held VIP whose port drops connections: %v", err)
	}
}

func TestAJoinerWithAnotherSecretStops(t *testing.T) {
	w := single(t)
	w.start(t)
	other := newWorld(t, "lb-2", netip.MustParsePrefix("10.0.0.12/24"), "52:54:00:00:00:12", "127.0.0.1:0", "", w.net.vip, w.net.macs)
	other.cfg.Bootstrap = false
	s, _ := secret.Generate()
	b, _ := secret.Generate()
	_ = secret.CreateExclusive(other.cfg.SecretFile, s)
	_ = secret.CreateExclusive(other.cfg.BackendSecretFile, b)
	err := Run(context.Background(), other.cfg, other.deps())
	if err == nil || !strings.Contains(err.Error(), "did not accept the secret in "+other.cfg.SecretFile+": it is out of date, or belongs to another cluster") ||
		!strings.Contains(err.Error(), "sudo thelb admin secret show --role director") {
		t.Errorf("a joiner with another secret: %v", err)
	}
	if !strings.Contains(w.logs.String(), "WARN join refused: the directors' secret was not proved") {
		t.Errorf("the leader did not log the refusal:\n%s", w.logs)
	}
}

func TestTheControlVIPMustBeInANetworkOfThisNode(t *testing.T) {
	saved := hostnet.List
	hostnet.List = func() ([]hostnet.Address, error) {
		return []hostnet.Address{{Interface: "eth1", MAC: "52:54:00:00:00:02", Prefix: netip.MustParsePrefix("10.0.0.11/24")}}, nil
	}
	t.Cleanup(func() { hostnet.List = saved })
	w := single(t)
	w.cfg.ControlVIP = netip.MustParseAddr("192.168.50.10")
	d := w.deps()
	d.Local = nil
	if err := Run(context.Background(), w.cfg, d); err == nil || !strings.Contains(err.Error(), "--interface") {
		t.Errorf("a VIP in no network of this node: %v", err)
	}
}
