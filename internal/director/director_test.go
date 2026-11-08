package director

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/fingerprint"
	"github.com/dmartsapp/thelb/internal/hostnet"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/secret"
)

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
		{false, true, probeNotChecked, actCreate},
		{true, true, probeNotChecked, actBootstrap},
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

type world struct {
	dir     string
	run     string
	cfg     Config
	logs    *syncBuffer
	started chan struct{}
}

func newWorld(t *testing.T) *world {
	t.Helper()
	savedList, savedOwner := hostnet.List, secret.CheckOwner
	hostnet.List = func() ([]hostnet.Address, error) {
		return []hostnet.Address{{Interface: "eth1", MAC: "52:54:00:00:00:02", Prefix: netip.MustParsePrefix("10.0.0.11/24")}}, nil
	}
	secret.CheckOwner = func(os.FileInfo) error { return nil }
	t.Cleanup(func() { hostnet.List, secret.CheckOwner = savedList, savedOwner })

	base, err := os.MkdirTemp("/tmp", "thelb-dir-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	return &world{
		dir: base,
		run: filepath.Join(base, "run"),
		cfg: Config{
			ControlVIP: netip.MustParseAddr("10.0.0.10"), ControlPort: 7443,
			SecretFile: filepath.Join(base, "director.secret"), BackendSecretFile: filepath.Join(base, "backend.secret"),
			Bootstrap: true, Cluster: "prod", Name: "lb-1",
		},
		logs: &syncBuffer{},
	}
}

func (w *world) deps() Deps {
	w.started = make(chan struct{})
	id, _ := fingerprint.ParseMachineID("4c2f3a0e9b7d4e1fa6c58d2b91e0f7a3")
	return Deps{
		RunDir: w.run,
		Log:    logx.New(w.logs, "thelb"),
		Now:    time.Now,
		Host: func(mac string) (fingerprint.Inputs, error) {
			return fingerprint.Inputs{MachineID: id, MAC: mac, Hostname: "lb-1"}, nil
		},
		PeerUID: func(*net.UnixConn) (int, error) { return 0, nil },
		Ready:   func() { close(w.started) },
	}
}

// start runs a director until the test ends, and waits for it to be ready.
func (w *world) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	d := w.deps() // before the goroutine: it makes the channel waited on below
	go func() { errc <- Run(ctx, w.cfg, d) }()
	select {
	case <-w.started:
	case err := <-errc:
		cancel()
		t.Fatalf("the director stopped: %v\n%s", err, w.logs)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the director did not get ready")
	}
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("the director ended with %v", err)
		}
	})
}

func TestBootstrapCreatesTheSecretsAndACluster(t *testing.T) {
	w := newWorld(t)
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

	resp, err := localsock.Call(w.run, localsock.Request{Op: localsock.OpStatus})
	if err != nil {
		t.Fatal(err)
	}
	st := resp.Status
	if st.Cluster == nil || st.Cluster.Name != "prod" || len(st.Cluster.ID) != 8 || st.Cluster.Leader != "lb-1" || st.Cluster.Term != nil {
		t.Errorf("cluster %+v", st.Cluster)
	}
	if len(st.Nodes) != 1 || st.Nodes[0].Address != "10.0.0.11" || len(st.Nodes[0].NodeFingerprint) != 16 {
		t.Errorf("nodes %+v", st.Nodes)
	}
	if len(st.Certificates) != 2 || st.Certificates[0].Kind != "ca" || st.Certificates[1].Certificate != "lb-1" {
		t.Errorf("certificates %+v", st.Certificates)
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

	for _, line := range []string{"OK secret file created role=director", "OK secret file created role=backend", "OK cluster created cluster=prod", "OK certificate authority created", "OK director ready", "WARN control VIP not checked on the network"} {
		if !strings.Contains(w.logs.String(), line) {
			t.Errorf("the log lacks %q:\n%s", line, w.logs)
		}
	}
}

func TestNoSecretEverReachesTheLog(t *testing.T) {
	w := newWorld(t)
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
	w := newWorld(t)
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
	w := newWorld(t)
	w.start(t)
	err := Run(context.Background(), w.cfg, w.deps())
	if !errors.Is(err, localsock.ErrAlreadyRunning) {
		t.Errorf("a second director: %v, want ErrAlreadyRunning", err)
	}
}

func TestWithoutBootstrap(t *testing.T) {
	w := newWorld(t)
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
	if err := Run(context.Background(), w.cfg, w.deps()); !errors.Is(err, ErrJoinNotYet) {
		t.Errorf("a file, no --bootstrap: %v, want ErrJoinNotYet", err)
	}
	if _, err := os.Stat(w.run); !os.IsNotExist(err) {
		t.Errorf("the run directory was left behind: %v", err)
	}
}

func TestStoppingLeavesNoSocket(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	d := w.deps()
	go func() { errc <- Run(ctx, w.cfg, d) }()
	<-w.started
	cancel()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.run); !os.IsNotExist(err) {
		t.Errorf("the run directory is still there: %v", err)
	}
	if !strings.Contains(w.logs.String(), "OK stopped") {
		t.Errorf("no stop in the log:\n%s", w.logs)
	}
}

func TestTheControlVIPMustBeInANetworkOfThisNode(t *testing.T) {
	w := newWorld(t)
	w.cfg.ControlVIP = netip.MustParseAddr("192.168.50.10")
	if err := Run(context.Background(), w.cfg, w.deps()); err == nil || !strings.Contains(err.Error(), "--interface") {
		t.Errorf("a VIP in no network of this node: %v", err)
	}
}
