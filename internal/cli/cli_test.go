package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmartsapp/thelb/internal/director"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/secret"
	"github.com/dmartsapp/thelb/internal/status"
)

// as runs the command line as the given user, on Linux.
func as(t *testing.T, euid int, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	savedEUID, savedOS := geteuid, goos
	geteuid = func() int { return euid }
	goos = "linux"
	t.Cleanup(func() { geteuid, goos = savedEUID, savedOS })
	var out, errOut bytes.Buffer
	code = Main(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestHelpAndVersionRunForAnyone(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"-h"}, {"help"}, {"help", "director"}, {"director", "--help"}, {"status", "-h"}} {
		code, out, _ := as(t, 1000, args...)
		if code != 0 || !strings.Contains(out, "Usage:") {
			t.Errorf("%v as a user: exit %d, output %q", args, code, out)
		}
	}
	code, out, _ := as(t, 1000, "--version")
	if code != 0 || !strings.HasPrefix(out, "thelb ") || !strings.Contains(out, "go1.") {
		t.Errorf("--version: exit %d, %q", code, out)
	}
}

func TestEverythingElseNeedsRoot(t *testing.T) {
	for _, args := range [][]string{{"director"}, {"status"}, {"generate", "secret"}, {"admin", "secret", "show", "--role", "director"}, {"doctor"}, {"backend"}} {
		code, _, errOut := as(t, 1000, args...)
		if code != 1 || !strings.Contains(errOut, "runs only as root") {
			t.Errorf("%v as a user: exit %d, %q", args, code, errOut)
		}
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{
		{"reboot"},
		{"--frobnicate"},
		{"help", "nope"},
		{"director", "--no-such-flag"},
		{"director", "--port", "7443", "--secret-file", "/s", "--backend-secret-file", "/b"},
		{"director", "--vip", "10.0.0.10/24", "--port", "7443", "--secret-file", "/s", "--backend-secret-file", "/b"},
		{"director", "--vip", "ten", "--port", "7443", "--secret-file", "/s", "--backend-secret-file", "/b"},
		{"director", "--vip", "10.0.0.10", "--port", "0", "--secret-file", "/s", "--backend-secret-file", "/b"},
		{"director", "--vip", "10.0.0.10", "--port", "7443", "--secret-file", "rel/s", "--backend-secret-file", "/b"},
		{"director", "--vip", "10.0.0.10", "--port", "7443", "--secret-file", "/s", "--backend-secret-file", "/s"},
		{"director", "--vip", "10.0.0.10", "--port", "7443", "--secret-file", "/s"},
		{"director", "--vip", "10.0.0.10", "--port", "7443", "--secret-file", "/s", "--backend-secret-file", "/b", "--cluster", "Prod"},
		{"director", "--vip", "10.0.0.10", "--port", "7443", "--secret-file", "/s", "--backend-secret-file", "/b", "--name", "LB_1"},
		{"status", "secrets"},
		{"generate"},
		{"generate", "password"},
		{"admin"},
		{"admin", "secret", "show"},
		{"admin", "secret", "show", "--role", "root"},
	} {
		code, _, errOut := as(t, 0, args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, code, errOut)
		}
	}
}

func TestNotYetExitsOne(t *testing.T) {
	for _, args := range [][]string{{"backend"}, {"doctor"}, {"generate", "systemd"}, {"admin", "approve", "lb-2"}, {"admin", "secret", "rotate"}} {
		code, _, errOut := as(t, 0, args...)
		if code != 1 || !strings.Contains(errOut, "not in this build") {
			t.Errorf("%v: exit %d, %q", args, code, errOut)
		}
	}
}

func TestDirectorFlagsReachTheDirector(t *testing.T) {
	var got director.Config
	saved := runDirector
	runDirector = func(_ context.Context, cfg director.Config, d director.Deps) error {
		got = cfg
		if d.RunDir != runDir || d.Log == nil || d.Host == nil {
			t.Errorf("deps %+v", d)
		}
		return nil
	}
	t.Cleanup(func() { runDirector = saved })
	code, _, errOut := as(t, 0, "director", "--vip", "10.0.0.10", "--port=7443", "--secret-file", "/etc/thelb/director.secret",
		"--backend-secret-file", "/etc/thelb/backend.secret", "--bootstrap", "--cluster", "prod", "--name", "lb-1", "--interface", "eth1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got.ControlVIP.String() != "10.0.0.10" || got.ControlPort != 7443 || !got.Bootstrap || got.Cluster != "prod" || got.Name != "lb-1" ||
		got.Interface != "eth1" || got.SecretFile != "/etc/thelb/director.secret" || got.BackendSecretFile != "/etc/thelb/backend.secret" {
		t.Errorf("config %+v", got)
	}
}

func TestTheDirectorRunsOnlyOnLinux(t *testing.T) {
	savedEUID, savedOS := geteuid, goos
	geteuid, goos = func() int { return 0 }, "darwin"
	t.Cleanup(func() { geteuid, goos = savedEUID, savedOS })
	var out, errOut bytes.Buffer
	code := Main(context.Background(), []string{"director", "--vip", "10.0.0.10", "--port", "7443", "--secret-file", "/s", "--backend-secret-file", "/b", "--name", "x"}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "only on Linux") {
		t.Errorf("exit %d, %q", code, errOut.String())
	}
}

func TestGenerateSecret(t *testing.T) {
	code, out, _ := as(t, 0, "generate", "secret")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := secret.Parse(out); err != nil {
		t.Errorf("not a secret: %q: %v", out, err)
	}
}

type fakeNode struct{}

func (fakeNode) Status() status.Status {
	return status.Status{View: "director", Cluster: &status.Cluster{Name: "prod", ID: "7f3c91a2", Leader: "lb-1"},
		Nodes: []status.Node{{Node: "lb-1", Role: "leader", Address: "10.0.0.11", State: "up"}}}
}

func (fakeNode) SecretText(role string) (string, error) {
	if role == "director" {
		return "thelb1:abc", nil
	}
	return "", errors.New("no")
}

func withRunningTheLB(t *testing.T) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "thelb-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	saved := runDir
	runDir = filepath.Join(base, "run")
	t.Cleanup(func() { runDir = saved })
	srv, err := localsock.Listen(runDir, fakeNode{}, logx.New(&bytes.Buffer{}, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	srv.PeerUID = func(*net.UnixConn) (int, error) { return 0, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; srv.Close() })
}

func TestStatusAsTablesAndJSON(t *testing.T) {
	withRunningTheLB(t)
	code, out, errOut := as(t, 0, "status")
	if code != 0 || !strings.Contains(out, " prod    | 7f3c91a2 |") || !strings.Contains(out, "(0 rows)") {
		t.Errorf("status: exit %d, %s %s", code, out, errOut)
	}
	code, out, _ = as(t, 0, "status", "nodes")
	if code != 0 || !strings.HasPrefix(out, " node ") || strings.Contains(out, "cluster") {
		t.Errorf("status nodes: %s", out)
	}
	code, out, _ = as(t, 0, "status", "--json")
	var st status.Status
	if code != 0 || json.Unmarshal([]byte(out), &st) != nil || st.Cluster.Name != "prod" || st.Pools == nil {
		t.Errorf("status --json: exit %d, %s", code, out)
	}
	code, out, _ = as(t, 0, "admin", "secret", "show", "--role", "director")
	if code != 0 || strings.TrimSpace(out) != "thelb1:abc" {
		t.Errorf("secret show: exit %d, %q", code, out)
	}
}

func TestStatusWithNothingRunning(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "thelb-cli-") // short: a socket path is limited to about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	saved := runDir
	runDir = filepath.Join(base, "run")
	t.Cleanup(func() { runDir = saved })
	code, _, errOut := as(t, 0, "status")
	if code != 1 || !strings.Contains(errOut, "not running on this node") {
		t.Errorf("exit %d, %q", code, errOut)
	}
}
