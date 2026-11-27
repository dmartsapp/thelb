// Package blackbox runs the theLB binary as a user would - knowing nothing of
// its code - with every command, every flag the help lists, and every hostile
// value we could think of. It finds the commands and flags in the binary's own
// help, so a new flag is tried the day it is added.
//
//	go test ./test/blackbox                       builds ./cmd/thelb, runs as you
//	THELB_BIN=bin/thelb go test ./test/blackbox   that binary instead
//	sudo THELB_BIN=... ./blackbox.test            as root too (the security lab
//	                                              runs it so, in a namespace of
//	                                              its own): the checks past the
//	                                              root check, never a daemon
//
// Whatever it is given, theLB must: end within 10 seconds; exit 0, 1 or 2;
// never print a Go panic or a stack trace; never print a control character
// (but newline and tab); and on exit 2, name the help page to read.
package blackbox

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
)

var bin string

func TestMain(m *testing.M) {
	bin = os.Getenv("THELB_BIN")
	if bin == "" {
		dir, err := os.MkdirTemp("", "thelb-blackbox-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		bin = filepath.Join(dir, "thelb")
		build := exec.Command("go", "build", "-o", bin, "../../cmd/thelb")
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "building thelb:", err)
			os.Exit(1)
		}
		code := m.Run()
		_ = os.RemoveAll(dir)
		os.Exit(code)
	}
	if abs, err := filepath.Abs(bin); err == nil {
		bin = abs
	}
	os.Exit(m.Run())
}

type result struct {
	args           []string
	code           int
	stdout, stderr string
	hung           bool
}

func run(args ...string) result {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Stdin = strings.NewReader("") // no terminal: nothing waits for a "y"
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + os.TempDir()}
	err := cmd.Run()
	r := result{args: args, stdout: out.String(), stderr: errb.String(), code: cmd.ProcessState.ExitCode()}
	if ctx.Err() != nil {
		r.hung = true
	}
	_ = err
	return r
}

// sane is what every run must be, whatever it was given.
func sane(t *testing.T, r result) {
	t.Helper()
	q := fmt.Sprintf("%q", r.args)
	if len(q) > 200 {
		q = q[:200] + "..."
	}
	switch {
	case r.hung:
		t.Errorf("thelb %s: still running after 10 s", q)
		return
	case r.code < 0 || r.code > 2:
		t.Errorf("thelb %s: exit %d\n%s", q, r.code, r.stderr)
	}
	for _, s := range []string{r.stdout, r.stderr} {
		for _, bad := range []string{"panic:", "goroutine ", "runtime error", "runtime.", ".go:"} {
			if strings.Contains(s, bad) {
				t.Errorf("thelb %s: %q in the output - a crash, or the code showing through:\n%s", q, bad, s)
			}
		}
		for _, c := range s {
			if c != '\n' && c != '\t' && (unicode.IsControl(c) || c == '\u2028' || c == '\u2029' || (c >= '\u202a' && c <= '\u202e') || (c >= '\u2066' && c <= '\u2069')) {
				t.Errorf("thelb %s: prints the control character %U", q, c)
				break
			}
		}
	}
	if r.code != 0 && strings.TrimSpace(r.stderr) == "" {
		t.Errorf("thelb %s: exit %d, saying nothing", q, r.code)
	}
	if r.code == 2 && !strings.Contains(r.stderr, " --help' for usage.") {
		t.Errorf("thelb %s: a usage error that names no help page:\n%s", q, r.stderr)
	}
	if r.code != 0 && strings.Count(strings.TrimSpace(r.stderr), "\n") > 8 && !strings.Contains(strings.Join(r.args, " "), "help") {
		t.Errorf("thelb %s: an error of %d lines:\n%s", q, strings.Count(r.stderr, "\n"), r.stderr)
	}
}

// commands are the command words, found in the help: each page's commands,
// and theirs.
func commands(t *testing.T) [][]string {
	t.Helper()
	var out [][]string
	seen := map[string]bool{}
	var walk func(path []string)
	line := regexp.MustCompile(`^  ([a-z0-9][a-z0-9-]*) {2,}\S`)
	walk = func(path []string) {
		r := run(append(append([]string{}, path...), "--help")...)
		if r.code != 0 {
			t.Fatalf("thelb %s --help: exit %d", strings.Join(path, " "), r.code)
		}
		out = append(out, path)
		section := ""
		for _, l := range strings.Split(r.stdout, "\n") {
			if strings.HasSuffix(l, ":") && !strings.HasPrefix(l, " ") {
				section = l
			}
			m := line.FindStringSubmatch(l)
			if m == nil || strings.Contains(section, "lags") || strings.Contains(section, "xample") || section == "" {
				continue
			}
			sub := append(append([]string{}, path...), m[1])
			if m[1] == "help" || len(sub) > 4 || seen[strings.Join(sub, " ")] {
				continue
			}
			// A word in a list of the help - status's views, offer's checks -
			// is not a command: a command's own page says it is.
			if page := run(append(append([]string{}, sub...), "--help")...); !strings.Contains(page.stdout, "Usage:\n  thelb "+strings.Join(sub, " ")) {
				continue
			}
			seen[strings.Join(sub, " ")] = true
			walk(sub)
		}
	}
	walk(nil)
	return out
}

// flags are a command's flags, from its help: name, and whether it takes a
// value.
func flags(path []string) map[string]bool {
	r := run(append(append([]string{}, path...), "--help")...)
	out := map[string]bool{}
	re := regexp.MustCompile(`^\s+(?:-[a-zA-Z0-9], )?--([a-z0-9][a-z0-9-]*)(?: (\S+))?\s{2,}`)
	sc := bufio.NewScanner(strings.NewReader(r.stdout))
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			out[m[1]] = m[2] != ""
		}
	}
	return out
}

// values are what each flag and argument is tried with.
var values = []string{
	"", " ", "0", "-1", "1", "65535", "65536", "99999999999999999999", "0x10", "1e3", "-0", "NaN", "true", "false", "yes",
	"10.0.0.10", "10.0.0.10/24", "0.0.0.0", "255.255.255.255", "224.0.0.1", "::", "::1", "fe80::1%eth0", "::ffff:10.0.0.1",
	"[::1]:80", "10.0.0.10:7443", "10.0.0.10:0", "10.0.0.10:99999", "localhost", "a..b", strings.Repeat("a", 250) + ".com",
	"/", "/etc/passwd", "../../etc/passwd", "relative/path", "/dev/null", "/dev/zero", "/proc/self/environ", "/nonexistent/x",
	"in-cluster", "-", "--", "--help", "-h", "=", "a=b", "a=b=c", "tcp/80", "web=tcp/8080,check=http:/x", "udp/53,check=tcp",
	"5s", "-5s", "0s", "1ns", "11m", "9999999h", "Ω", "日本", "\u200b", "a b", "$(id)", "`id`", "'", `"`, `\`, ";", "|", "&", "*",
	"?", "~", "%s%n", strings.Repeat("x", 4096), strings.Repeat("x", 4097), "a\nb", "\x1b[2J", "\u202eevil", "\x7f", "\xff\xfe",
}

// parallel runs fn over every item, a few at a time.
func parallel(n int, items [][]string, fn func([]string)) {
	ch := make(chan []string)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				fn(it)
			}
		}()
	}
	for _, it := range items {
		ch <- it
	}
	close(ch)
	wg.Wait()
}

// daemon is whether path runs a node: as root, the matrix never starts one -
// and with one flag at a time, a required flag is always missing.
func daemon(path []string) bool {
	return len(path) == 1 && (path[0] == "director" || path[0] == "backend")
}

// bases are command lines that hold every flag their command requires, for
// root's matrix: so a hostile value reaches the check of its own flag, not a
// missing one. Never a director or a backend - generate systemd checks their
// flags with their own parser.
var bases = map[string][]string{
	"generate systemd director": {"--vip", "10.0.0.10", "--port", "7443", "--secret-file", "/etc/thelb/d.secret", "--backend-secret-file", "/etc/thelb/b.secret"},
	"generate systemd backend":  {"--join", "10.0.0.10:7443", "--secret-file", "/etc/thelb/b.secret"},
	"generate k8s director":     {"--vip", "10.0.0.10", "--port", "7443", "--secret-file", "/etc/thelb/d.secret", "--backend-secret-file", "/etc/thelb/b.secret", "--kubeconfig", "in-cluster", "--metrics", "127.0.0.1:9180"},
	"generate k8s service":      {"web"},
	"admin pool add":            {"main", "--vip", "10.0.0.50/24", "--vippool-count", "4"},
	"admin service add":         {"web", "--proto", "tcp", "--port", "80"},
	"admin secret show":         {"--role", "director"},
}

// with replaces flag's value in base, or adds it.
func with(base []string, flag, value string) []string {
	out := append([]string{}, base...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "--"+flag {
			out[i+1] = value
			return out
		}
	}
	return append(out, "--"+flag, value)
}

// typed is whether a flag takes only values of a kind - a number, a duration,
// a map - so that the parser itself refuses the rest.
func typed(path []string, flag string) bool {
	r := run(append(append([]string{}, path...), "--"+flag, "not-a-value-of-any-kind")...)
	return r.code == 2 && strings.Contains(r.stderr, "invalid argument")
}

func TestEveryCommandEveryFlagEveryValue(t *testing.T) {
	root := os.Geteuid() == 0
	sample := []string{"", "0", "/etc/passwd", "Ω", strings.Repeat("x", 4097), "a\nb"}
	var cases [][]string
	add := func(path []string, more ...string) {
		cases = append(cases, append(append([]string{}, path...), more...))
	}
	for _, path := range commands(t) {
		if daemon(path) && root {
			continue // generate systemd director|backend checks its flags, as root
		}
		if len(path) > 0 && path[0] == "doctor" && root {
			continue // as root, doctor reads the machine for seconds; TestAsRoot has a few
		}
		add(path)
		add(path, "--no-such-flag")
		for _, v := range sample {
			add(path, v)
		}
		key := strings.Join(path, " ")
		base, hasBase := bases[key]
		for name, takes := range flags(path) {
			if name == "help" || name == "version" {
				continue
			}
			if !takes {
				add(path, "--"+name)
				add(path, "--"+name+"=maybe")
				continue
			}
			add(path, "--"+name)
			vs := sample
			if typed(path, name) {
				vs = values
			}
			for _, v := range vs {
				add(path, "--"+name, v)
				add(path, "--"+name+"="+v)
			}
			if root && hasBase {
				for _, v := range values {
					cases = append(cases, append(append([]string{}, path...), with(base, name, v)...))
				}
			}
		}
		if root && hasBase {
			for _, v := range values {
				cases = append(cases, append(append(append([]string{}, path...), base...), v))
			}
		}
	}
	if testing.Short() {
		var few [][]string
		for i := 0; i < len(cases); i += 7 {
			few = append(few, cases[i])
		}
		cases = few
	}
	t.Logf("%d runs of thelb, as %s", len(cases), map[bool]string{true: "root", false: "a user"}[root])
	var mu sync.Mutex
	parallel(2*runtime.NumCPU(), cases, func(args []string) {
		r := run(args...)
		mu.Lock()
		defer mu.Unlock()
		sane(t, r)
		if !root && r.code == 0 && !help(args) {
			t.Errorf("thelb %q: as a user, exit 0", args)
		}
	})
}

func help(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" || a == "help" || a == "completion" || a == "--version" || a == "-v" {
			return true
		}
	}
	// A command that groups others shows its page alone; that is fine.
	return len(args) <= 3
}

// TestAsAUser: everything but the help, --version and completion is root's -
// and says so, before reading any file or flag value it would check.
func TestAsAUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root")
	}
	for _, args := range [][]string{
		{"status"}, {"doctor"}, {"generate", "secret"}, {"admin", "approve", "x"}, {"offer", "remove", "x"},
		{"director", "--vip", "10.0.0.10", "--port", "7443", "--secret-file", "/etc/shadow", "--backend-secret-file", "/etc/gshadow"},
		{"generate", "k8s", "kubeconfig", "--from-kubeconfig", "/etc/shadow", "--yes"},
	} {
		r := run(args...)
		sane(t, r)
		if r.code != 1 || !strings.Contains(r.stderr, "runs only as root") {
			t.Errorf("thelb %q as a user: exit %d, %q", args, r.code, r.stderr)
		}
		if strings.Contains(r.stderr, "shadow") && !strings.Contains(r.stderr, "runs only as root") {
			t.Errorf("thelb %q as a user read a file it was given: %q", args, r.stderr)
		}
	}
	for _, args := range [][]string{{}, {"--help"}, {"-h"}, {"help"}, {"--version"}, {"-v"}, {"completion", "bash"}, {"completion", "zsh"}, {"help", "admin", "pool", "add"}} {
		r := run(args...)
		sane(t, r)
		if r.code != 0 {
			t.Errorf("thelb %q as a user: exit %d", args, r.code)
		}
	}
}

// TestAsRoot: past the root check, as root - in a namespace of its own, with
// nothing of theLB running: what generate prints, and what it refuses.
func TestAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("not root")
	}
	sec := t.TempDir()
	good := filepath.Join(sec, "d.secret")
	r := run("generate", "secret")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "thelb1:") {
		t.Fatalf("generate secret: exit %d %q", r.code, r.stdout)
	}
	if err := os.WriteFile(good, []byte(r.stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := []string{"--vip", "10.0.0.10", "--port", "7443", "--secret-file", "/etc/thelb/d.secret", "--backend-secret-file", "/etc/thelb/b.secret"}
	for _, c := range []struct {
		args []string
		code int
		says string
	}{
		{append([]string{"generate", "systemd", "director"}, dir...), 0, "ExecStart="},
		{append(append([]string{"generate", "systemd", "director"}, dir...), "--interface", "eth0;id"), 2, "not an interface's name"},
		{append(append([]string{"generate", "systemd", "director"}, dir...), "--metrics", "0.0.0.0:9"), 2, "not 0.0.0.0"},
		{append(append([]string{"generate", "systemd", "director"}, dir...), "--kubeconfig", "rel/kc"), 2, "absolute path"},
		{append(append([]string{"generate", "systemd", "director"}, dir...), "--bootstrap-wait", "40s"), 2, "give --bootstrap too"},
		{append(append([]string{"generate", "systemd", "director"}, dir...), "--bootstrap", "--bootstrap-wait", "11m"), 2, "5s to 10m"},
		{append(append([]string{"generate", "systemd", "director"}, dir...), "--cluster", "Prod"), 2, "cluster's name"},
		{append(append([]string{"generate", "systemd", "director"}, dir...), "--name", "LB_1"), 2, "node's name"},
		{[]string{"generate", "systemd", "director", "--vip", "10.0.0.10/24", "--port", "7443", "--secret-file", "/s", "--backend-secret-file", "/b"}, 2, "without a prefix"},
		{[]string{"generate", "systemd", "director", "--vip", "10.0.0.10", "--port", "7443", "--secret-file", "/s", "--backend-secret-file", "/s"}, 2, "two files"},
		{[]string{"generate", "systemd", "backend", "--join", "10.0.0.10:7443", "--secret-file", "/b", "--offer", "tcp/80", "--offer", "tcp/80"}, 2, "offered twice"},
		{[]string{"generate", "systemd", "backend", "--join", "10.0.0.10", "--secret-file", "/b"}, 2, "--join is the control VIP"},
		{[]string{"generate", "k8s", "service", "WEB"}, 2, "Service's name"},
		{[]string{"generate", "k8s", "service", "web", "--port", "80:0"}, 2, "1 to 65535"},
		{[]string{"generate", "k8s", "service", "web", "--selector", "a=b,c=d"}, 2, "one label"},
		{[]string{"generate", "k8s", "service", "web", "--namespace", "../kube-system"}, 2, "namespace"},
		{[]string{"generate", "k8s", "node", "a_b"}, 2, "node's name"},
		{[]string{"generate", "k8s", "kubeconfig", "--from-kubeconfig", "/dev/zero"}, 1, ""},
		{[]string{"generate", "k8s", "kubeconfig", "--from-kubeconfig", good}, 1, ""},
		{[]string{"generate", "sysctl", "router"}, 2, "director or backend"},
		{[]string{"status"}, 1, "no director runs|not running"},
		{[]string{"status", "--watch"}, 1, ""},
		{[]string{"admin", "approve", "--match", "a(", "--yes"}, 2, "--match"},
		{[]string{"admin", "lease", "1"}, 2, "whole seconds"},
		{[]string{"admin", "lease", "3601"}, 2, "whole seconds"},
		{[]string{"admin", "secret", "show", "--role", "root"}, 2, "director or backend"},
		{[]string{"admin", "pool", "add", "main", "--vip", "10.0.0.50", "--vippool-count", "4"}, 2, "prefix"},
		{[]string{"offer", "add", "web", "tcp/80,check=http:/a b"}, 2, ""},
		{[]string{"doctor", "--role", "router"}, 2, "director or backend"},
		{[]string{"doctor", "--vip", "x"}, 2, "address"},
		{[]string{"doctor", "--json"}, -1, ""},
	} {
		r := run(c.args...)
		sane(t, r)
		if (c.code >= 0 && r.code != c.code) || (c.says != "" && !regexp.MustCompile(c.says).MatchString(r.stderr+r.stdout)) {
			t.Errorf("thelb %q as root: exit %d %q; wanted %d saying %q", c.args, r.code, firstLine(r.stderr), c.code, c.says)
		}
	}
	// What generate prints holds no control character - sane checks - and no
	// value given to it unquoted where a shell or systemd would read it.
	r = run(append(append([]string{"generate", "systemd", "director"}, dir...), "--name", "lb-1")...)
	if !strings.Contains(r.stdout, "ExecStart=") || strings.Contains(r.stdout, "$(") {
		t.Errorf("generate systemd: %s", r.stdout)
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
