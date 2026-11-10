package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/hostnet"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/backend"
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
		{"director", "--vip", "not a name", "--port", "7443", "--secret-file", "/s", "--backend-secret-file", "/b"},
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
		{"admin", "approve"},
		{"admin", "approve", "--match", "web-("},
		{"admin", "approve", "--yes", "web-1"},
		{"admin", "approve", "--match", "web-.*", "web-1"},
		{"admin", "refuse"},
		{"admin", "remove", "web-1", "web-2"},
		{"backend", "--join", "10.0.0.10", "--secret-file", "/b"},
		{"backend", "--join", "10.0.0.10:7443"},
		{"backend", "--join", "10.0.0.10:7443", "--secret-file", "/b", "--offer", "tcp/80,check=icmp"},
		{"backend", "--join", "10.0.0.10:7443", "--secret-file", "/b", "--offer", "udp/53,check=tcp"},
		{"backend", "--join", "10.0.0.10:7443", "--secret-file", "/b", "--offer", "tcp/80", "--offer", "tcp/80"},
		{"admin", "pool", "add", "external", "--vip", "10.10.1.20", "--vippool-count", "10"},
		{"admin", "pool", "add", "external", "--vip", "10.10.1.20/24"},
		{"admin", "service", "add", "nginx", "--port", "80"},
		{"admin", "assign", "nginx"},
		{"admin", "assign", "nginx", "web-1:tcp/80", "--weight", "web-1"},
		{"admin", "unassign", "nginx"},
	} {
		code, _, errOut := as(t, 0, args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, code, errOut)
		}
	}
}

func TestNotYetExitsOne(t *testing.T) {
	for _, args := range [][]string{{"doctor"}, {"generate", "systemd"}, {"admin", "secret", "rotate"}} {
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
		if d.RunDir != filepath.Join(runDir, "director") || d.Log == nil || d.Host == nil {
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
		Nodes: []status.Node{{Node: "lb-1", Role: "leader", Address: "10.0.0.11", State: "up"}},
		Pending: []status.Pending{
			{Pending: "web-1", Role: "director", Address: "10.0.0.21", MAC: "52:54:00:00:00:21", NodeFingerprint: "0be98d17c46a2f83", WaitingSeconds: 15},
			{Pending: "web-12", Role: "director", Address: "10.0.0.22", MAC: "52:54:00:00:00:22", NodeFingerprint: "9f12c4e07ab35d61", WaitingSeconds: 9},
			{Pending: "db-1", Role: "director", Address: "10.0.0.31", MAC: "52:54:00:00:00:31", NodeFingerprint: "c5a8e3071bd92f64", WaitingSeconds: 3},
		}}
}

// adminCalls are the changes the fake node was asked for.
var adminCalls []string

func (fakeNode) Admin(c api.Change) (api.Result, error) {
	adminCalls = append(adminCalls, c.Op+" "+strings.Join(c.Names, ","))
	return api.Result{Names: c.Names}, nil
}

func (fakeNode) Offer(o api.Offer) (api.Result, error) {
	offerCalls = append(offerCalls, o.Op+" "+o.Offer.Name+" "+o.Offer.Key()+" "+o.Offer.Check)
	return api.Result{Message: "offer " + o.Offer.Name}, nil
}

var offerCalls []string

func (fakeNode) Generate(what string) (string, error) {
	if what == "commands" {
		return "#!/bin/sh\nthelb admin pool add lab --vip 10.0.0.20/24 --vippool-count 10\n", nil
	}
	return "", errors.New("no")
}

func (fakeNode) SecretText(role string) (string, error) {
	if role == "director" {
		return "thelb1:abc", nil
	}
	return "", errors.New("no")
}

func withRunningTheLB(t *testing.T) { t.Helper(); withRunning(t, "director") }

// withRunning runs the fake theLB as the role named.
func withRunning(t *testing.T, role string) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "thelb-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	saved := runDir
	runDir = filepath.Join(base, "run")
	t.Cleanup(func() { runDir = saved })
	srv, err := localsock.Listen(filepath.Join(runDir, role), fakeNode{}, logx.New(&bytes.Buffer{}, "socket"))
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
	if code != 0 || !strings.Contains(out, " prod    | 7f3c91a2 |") || !strings.Contains(out, " directors |     1 |") || !strings.Contains(out, "More: thelb status") {
		t.Errorf("status: exit %d, %s %s", code, out, errOut)
	}
	code, out, _ = as(t, 0, "status", "nodes")
	if code != 0 || !strings.HasPrefix(out, " node ") || strings.Contains(out, "cluster") {
		t.Errorf("status nodes: %s", out)
	}
	code, out, _ = as(t, 0, "status", "all")
	if code != 0 || strings.Contains(out, "rows)") || !strings.Contains(out, " pool ") {
		t.Errorf("status all: %s", out)
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

func TestApprove(t *testing.T) {
	withRunningTheLB(t)
	savedTerm, savedIn := isTerminal, stdin
	t.Cleanup(func() { isTerminal, stdin = savedTerm, savedIn })
	run := func(terminal bool, input string, args ...string) (int, string, string) {
		isTerminal = func() bool { return terminal }
		stdin = strings.NewReader(input)
		adminCalls = nil
		return as(t, 0, args...)
	}

	code, out, _ := run(false, "", "admin", "approve", "db-1")
	if code != 0 || out != "approved: db-1\n" || strings.Join(adminCalls, ";") != "approve db-1" {
		t.Errorf("by name: exit %d, %q, %v", code, out, adminCalls)
	}

	// --match is the whole name: web-1 and web-12 match 'web-[0-9]+', db-1 does not.
	code, out, errOut := run(false, "", "admin", "approve", "--match", "web-[0-9]+")
	if code != 1 || !strings.Contains(errOut, "no terminal to confirm on") || adminCalls != nil {
		t.Errorf("no terminal, no --yes: exit %d, %q, %v", code, errOut, adminCalls)
	}
	if !strings.Contains(out, " web-1   | director") || !strings.Contains(out, " web-12  | director") || strings.Contains(out, "db-1") || strings.Count(out, "| director") != 2 {
		t.Errorf("the matching nodes are not shown first:\n%s", out)
	}
	code, _, _ = run(true, "n\n", "admin", "approve", "--match", "web-[0-9]+")
	if code != 1 || adminCalls != nil {
		t.Errorf("answered no: exit %d, %v", code, adminCalls)
	}
	code, out, _ = run(true, "y\n", "admin", "approve", "--match", "web-[0-9]+")
	if code != 0 || strings.Join(adminCalls, ";") != "approve web-1,web-12" || !strings.Contains(out, "Approve these 2 nodes? [y/N] approved: web-1, web-12") {
		t.Errorf("answered yes: exit %d, %q, %v", code, out, adminCalls)
	}
	code, _, _ = run(false, "", "admin", "approve", "--match", "web-1", "--yes")
	if code != 0 || strings.Join(adminCalls, ";") != "approve web-1" {
		t.Errorf("--yes, matching web-1 only: exit %d, %v", code, adminCalls)
	}
	code, _, errOut = run(false, "", "admin", "approve", "--match", "lb-.*", "--yes")
	if code != 1 || !strings.Contains(errOut, "no pending node's name matches") || adminCalls != nil {
		t.Errorf("nothing matches: exit %d, %q", code, errOut)
	}

	for _, c := range []struct{ op, want string }{{"refuse", "refused: web-1\n"}, {"remove", "removed: web-1\n"}} {
		code, out, _ := run(false, "", "admin", c.op, "web-1")
		if code != 0 || out != c.want || strings.Join(adminCalls, ";") != c.op+" web-1" {
			t.Errorf("%s: exit %d, %q, %v", c.op, code, out, adminCalls)
		}
	}
}

func TestBackendFlagsReachTheBackend(t *testing.T) {
	var got backend.Config
	saved := runBackend
	runBackend = func(_ context.Context, cfg backend.Config, d backend.Deps) error {
		got = cfg
		if d.RunDir != filepath.Join(runDir, "backend") || d.Log == nil || d.Kernel == nil {
			t.Errorf("deps %+v", d)
		}
		return nil
	}
	t.Cleanup(func() { runBackend = saved })
	savedOS := goos
	goos = "linux"
	t.Cleanup(func() { goos = savedOS })
	code, _, errOut := as(t, 0, "backend", "--join", "10.0.0.10:7443", "--secret-file", "/etc/thelb/backend.secret", "--name", "web-1",
		"--offer", "shop=tcp/8080,check=http:/healthz", "--offer", "udp/53")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got.Join.String() != "10.0.0.10:7443" || got.Name != "web-1" || got.SecretFile != "/etc/thelb/backend.secret" || len(got.Offers) != 2 ||
		got.Offers[0].Check != "http:/healthz" || got.Offers[0].Name != "shop" || got.Offers[1].Check != "none" || got.Offers[1].Proto != "udp" ||
		got.Offers[1].Name != "udp-53" {
		t.Errorf("config %+v", got)
	}
}

func TestAdminChanges(t *testing.T) {
	withRunningTheLB(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"admin", "pool", "add", "external", "--vip", "10.10.1.20/24", "--vippool-count", "10"}, "pool.add "},
		{[]string{"admin", "service", "add", "nginx", "--pool", "external", "--proto", "tcp", "--port", "80"}, "service.add nginx"},
		{[]string{"admin", "assign", "nginx", "web-1:tcp/80", "web-2:tcp/80", "--weight", "web-1=3"}, "assign nginx"},
		{[]string{"admin", "unassign", "nginx", "web-2"}, "unassign nginx,web-2"},
		{[]string{"admin", "service", "remove", "nginx"}, "service.remove nginx"},
		{[]string{"admin", "pool", "remove", "external"}, "pool.remove "},
	} {
		adminCalls = nil
		code, _, errOut := as(t, 0, c.args...)
		if code != 0 || strings.Join(adminCalls, ";") != c.want {
			t.Errorf("%v: exit %d %s, calls %v", c.args, code, errOut, adminCalls)
		}
	}
}

func TestTheVIPMayBeAName(t *testing.T) {
	savedLookup, savedFor, savedRun, savedOS := lookup, localFor, runDirector, goos
	t.Cleanup(func() { lookup, localFor, runDirector, goos = savedLookup, savedFor, savedRun, savedOS })
	goos = "linux"
	answers := map[string][]netip.Addr{
		"thelb-vip": {netip.MustParseAddr("10.0.0.10")},
		"two-sites": {netip.MustParseAddr("192.168.9.10"), netip.MustParseAddr("10.0.0.10")},
		"elsewhere": {netip.MustParseAddr("192.168.9.10")},
		"ambiguous": {netip.MustParseAddr("10.0.0.10"), netip.MustParseAddr("10.0.0.11")},
	}
	lookup = func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if a, ok := answers[host]; ok {
			return a, nil
		}
		return nil, errors.New("no such host")
	}
	localFor = func(a netip.Addr, _ string) (hostnet.Address, error) {
		if netip.MustParsePrefix("10.0.0.0/24").Contains(a) {
			return hostnet.Address{}, nil
		}
		return hostnet.Address{}, errors.New("not here")
	}
	var got director.Config
	runDirector = func(_ context.Context, cfg director.Config, _ director.Deps) error { got = cfg; return nil }
	run := func(vip string) (int, string) {
		code, _, errOut := as(t, 0, "director", "--vip", vip, "--port", "7443", "--secret-file", "/s", "--backend-secret-file", "/b", "--name", "lb-1")
		return code, errOut
	}
	for _, name := range []string{"thelb-vip", "two-sites"} {
		got = director.Config{}
		if code, errOut := run(name); code != 0 || got.ControlVIP.String() != "10.0.0.10" || got.ControlVIPName != name {
			t.Errorf("%s: exit %d %s, config %+v", name, code, errOut, got)
		}
	}
	for name, want := range map[string]string{
		"elsewhere": "none of them is in a network of this node",
		"ambiguous": "more than one address in this node's networks",
		"nowhere":   "the name does not resolve",
	} {
		if code, errOut := run(name); code != 1 || !strings.Contains(errOut, want) {
			t.Errorf("%s: exit %d, %q", name, code, errOut)
		}
	}
	if code, _ := run("bad_name!"); code != 2 {
		t.Errorf("a malformed name: exit %d", code)
	}
}

func TestGenerateCommandsAsksTheDirector(t *testing.T) {
	withRunningTheLB(t)
	code, out, errOut := as(t, 0, "generate", "commands")
	if code != 0 || !strings.HasPrefix(out, "#!/bin/sh\n") || !strings.Contains(out, "pool add lab") {
		t.Errorf("exit %d, %q %q", code, out, errOut)
	}
}

// TestTheGeneratedCommandsAreCommands feeds every line director.Commands
// prints back to the command line: each must be a change it accepts.
func TestTheGeneratedCommandsAreCommands(t *testing.T) {
	withRunningTheLB(t)
	t0 := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	st := &cluster.State{
		Cluster: &cluster.Info{Name: "prod"},
		Nodes:   map[string]*cluster.Node{"lb-1": {}, "web-1": {}, "web-2": {}},
		Pools: map[string]*cluster.Pool{
			"main": {Name: "main", First: "10.0.0.20", Bits: 24, Count: 10, Default: true},
			"dmz":  {Name: "dmz", First: "192.168.50.10", Bits: 24, Count: 5, Interface: "eth1"},
		},
		Services: map[string]*cluster.Service{
			"web": {Name: "web", Pool: "main", VIP: "10.0.0.20", Proto: "tcp", Port: 80, Scheduler: "wrr"},
			"far": {Name: "far", Pool: "dmz", VIP: "192.168.50.10", Proto: "tcp", Port: 443, Scheduler: "mh", Method: "tunnel"},
		},
		Assignments: map[string]*cluster.Assignment{
			"web/web-1": {Service: "web", Node: "web-1", Proto: "tcp", Port: 8080, Weight: 3},
			"web/web-2": {Service: "web", Node: "web-2", Proto: "tcp", Port: 9090, Weight: 1},
			"far/web-1": {Service: "far", Node: "web-1", Proto: "tcp", Port: 443, Weight: 1},
		},
	}
	script := director.Commands(st, "lb-1", t0)
	for _, want := range []string{
		"# cluster prod again, as they were at 2026-11-09T10:00:00Z, on lb-1.",
		"#   thelb admin approve --match 'web-1|web-2'",
		"thelb admin pool add dmz --vip 192.168.50.10/24 --vippool-count 5 --interface eth1\n",
		"thelb admin pool add main --vip 10.0.0.20/24 --vippool-count 10 --default\n",
		"thelb admin service add far --pool dmz --vip 192.168.50.10 --proto tcp --port 443 --scheduler mh --method tunnel\n",
		"thelb admin service add web --pool main --vip 10.0.0.20 --proto tcp --port 80 --scheduler wrr --method direct\n",
		"thelb admin assign web web-1:tcp/8080 web-2:tcp/9090 --weight web-1=3\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q:\n%s", want, script)
		}
	}
	var ops []string
	for _, line := range strings.Split(script, "\n") {
		if !strings.HasPrefix(line, "thelb ") {
			continue
		}
		adminCalls = nil
		code, _, errOut := as(t, 0, strings.Fields(line)[1:]...)
		if code != 0 || len(adminCalls) != 1 {
			t.Errorf("%q: exit %d, %s, calls %v", line, code, errOut, adminCalls)
			continue
		}
		ops = append(ops, strings.Fields(adminCalls[0])[0])
	}
	if strings.Join(ops, " ") != "pool.add pool.add service.add service.add assign assign" {
		t.Errorf("the changes, in order: %v", ops)
	}
}

func TestOfferAsksTheBackend(t *testing.T) {
	withRunning(t, "backend")
	offerCalls = nil
	code, out, errOut := as(t, 0, "offer", "add", "shop", "tcp/8080,check=http:/healthz")
	if code != 0 || out != "offer shop\n" || strings.Join(offerCalls, ";") != "add shop tcp/8080 http:/healthz" {
		t.Errorf("add: exit %d, %q %q, %v", code, out, errOut, offerCalls)
	}
	offerCalls = nil
	code, _, errOut = as(t, 0, "offer", "add", "dns", "udp/53")
	if code != 0 || strings.Join(offerCalls, ";") != "add dns udp/53 none" {
		t.Errorf("add udp: exit %d, %q, %v", code, errOut, offerCalls)
	}
	offerCalls = nil
	code, _, errOut = as(t, 0, "offer", "remove", "shop")
	if code != 0 || strings.Join(offerCalls, ";") != "remove shop /0 " {
		t.Errorf("remove: exit %d, %q, %v", code, errOut, offerCalls)
	}
	for _, args := range [][]string{
		{"offer"},
		{"offer", "add", "shop"},
		{"offer", "add", "tcp/8080", "shop"},
		{"offer", "add", "shop", "web=tcp/8080"},
		{"offer", "add", "bad/name", "tcp/8080"},
		{"offer", "add", "shop", "tcp/8080,check=http:/", "extra"},
		{"offer", "remove"},
		{"offer", "list"},
	} {
		offerCalls = nil
		if code, _, _ := as(t, 0, args...); code != 2 || len(offerCalls) != 0 {
			t.Errorf("%v: exit %d, calls %v", args, code, offerCalls)
		}
	}
}
