// Package cli is theLB's command line: six commands, `--version`, and the help.
// Every command runs only as root (effective user ID 0); the help and
// `--version` run for anyone. Exit statuses: 0 done, 1 failed, 2 used wrongly.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/dmartsapp/thelb/internal/ctlnet"
	"github.com/dmartsapp/thelb/internal/director"
	"github.com/dmartsapp/thelb/internal/fingerprint"
	"github.com/dmartsapp/thelb/internal/hostnet"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/secret"
	"github.com/dmartsapp/thelb/internal/status"
	"github.com/dmartsapp/thelb/internal/version"
)

// What the command line reads the world through; tests replace these.
var (
	geteuid     = os.Geteuid
	goos        = runtime.GOOS
	runDir      = "/run/thelb"
	now         = time.Now
	hostInputs  = fingerprint.ReadHost
	runDirector = director.Run
	logOutput   io.Writer // nil: standard error
	stdin       io.Reader = os.Stdin
	isTerminal            = func() bool {
		fi, err := os.Stdin.Stat()
		return err == nil && fi.Mode()&os.ModeCharDevice != 0
	}
)

// usageError is a mistake in how theLB was called: exit status 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// notYet is a command this build does not have.
func notYet(what string) error {
	return fmt.Errorf("%s is not in this build of theLB yet", what)
}

// Main runs the command line and returns the exit status.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	err := run(ctx, args, stdout, stderr)
	if err == nil {
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "thelb: "+err.Error())
	var u usageError
	if errors.As(err, &u) {
		_, _ = fmt.Fprintln(stderr, `run "thelb --help" for the commands, or "thelb <command> --help" for one`)
		return 2
	}
	return 1
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stdout, helpText)
		return nil
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		if len(args) > 1 {
			return commandHelp(args[1], stdout)
		}
		_, _ = fmt.Fprint(stdout, helpText)
		return nil
	case "-v", "-version", "--version":
		_, _ = fmt.Fprintln(stdout, version.String())
		return nil
	}
	cmd, rest := args[0], args[1:]
	if _, ok := commandHelps[cmd]; !ok {
		return usagef("%q is not a theLB command", cmd)
	}
	for _, a := range rest {
		if a == "-h" || a == "-help" || a == "--help" {
			return commandHelp(cmd, stdout)
		}
	}
	if geteuid() != 0 {
		return fmt.Errorf("thelb %s runs only as root (effective user ID 0); the help and --version run for anyone", cmd)
	}
	switch cmd {
	case "director":
		return runDirectorCmd(ctx, rest, stderr)
	case "backend":
		return notYet("the backend")
	case "status":
		return statusCmd(ctx, rest, stdout)
	case "doctor":
		return notYet("thelb doctor")
	case "generate":
		return generateCmd(rest, stdout)
	default: // admin
		return adminCmd(rest, stdout)
	}
}

func commandHelp(cmd string, stdout io.Writer) error {
	h, ok := commandHelps[cmd]
	if !ok {
		return usagef("%q is not a theLB command", cmd)
	}
	_, _ = fmt.Fprint(stdout, h)
	return nil
}

// newFlags is a flag set that reports mistakes as usage errors, not by printing.
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return usagef("thelb %s: %v", fs.Name(), err)
	}
	return nil
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func runDirectorCmd(ctx context.Context, args []string, stderr io.Writer) error {
	fs := newFlags("director")
	vip := fs.String("vip", "", "")
	port := fs.Int("port", 0, "")
	secretFile := fs.String("secret-file", "", "")
	backendSecretFile := fs.String("backend-secret-file", "", "")
	bootstrap := fs.Bool("bootstrap", false, "")
	cluster := fs.String("cluster", "thelb", "")
	name := fs.String("name", "", "")
	iface := fs.String("interface", "", "")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("thelb director: unexpected %q", fs.Arg(0))
	}

	var cfg director.Config
	switch {
	case *vip == "":
		return usagef("thelb director: --vip, the control VIP, is required")
	case strings.Contains(*vip, "/"):
		return usagef("thelb director: --vip is the control VIP's address, without a prefix: %q", *vip)
	}
	addr, err := netip.ParseAddr(*vip)
	if err != nil {
		return usagef("thelb director: --vip %q is not an IP address", *vip)
	}
	cfg.ControlVIP = addr.Unmap()
	if *port < 1 || *port > 65535 {
		return usagef("thelb director: --port, the control port, must be 1 to 65535")
	}
	cfg.ControlPort = uint16(*port)
	for _, f := range []struct{ flag, path string }{{"--secret-file", *secretFile}, {"--backend-secret-file", *backendSecretFile}} {
		if f.path == "" {
			return usagef("thelb director: %s is required", f.flag)
		}
		if !filepath.IsAbs(f.path) {
			return usagef("thelb director: %s must be an absolute path: %q", f.flag, f.path)
		}
	}
	if filepath.Clean(*secretFile) == filepath.Clean(*backendSecretFile) {
		return usagef("thelb director: the directors' and the backends' secrets need two files")
	}
	cfg.SecretFile, cfg.BackendSecretFile = filepath.Clean(*secretFile), filepath.Clean(*backendSecretFile)
	if !nameRE.MatchString(*cluster) {
		return usagef("thelb director: --cluster %q: a cluster's name is lowercase letters, digits and hyphens, up to 63", *cluster)
	}
	cfg.Cluster = *cluster
	cfg.Name = *name
	if cfg.Name == "" {
		h, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("thelb director: this node's hostname: %w; name it with --name", err)
		}
		cfg.Name = strings.ToLower(strings.SplitN(h, ".", 2)[0])
	}
	if !nameRE.MatchString(cfg.Name) {
		return usagef("thelb director: the node's name %q: a node's name is lowercase letters, digits and hyphens, up to 63 - name it with --name", cfg.Name)
	}
	cfg.Bootstrap = *bootstrap
	cfg.Interface = *iface

	if goos != "linux" {
		return errors.New("thelb director runs only on Linux")
	}
	out := logOutput
	if out == nil {
		out = stderr
	}
	log := logx.New(out, "thelb")
	return runDirector(ctx, cfg, director.Deps{
		RunDir: runDir,
		Log:    log,
		Now:    now,
		Host:   hostInputs,
		Network: func(local hostnet.Address) (director.Network, error) {
			n, err := ctlnet.New(cfg.ControlVIP, cfg.ControlPort, local, log.Module("network"))
			if err != nil {
				return nil, err
			}
			return n, nil
		},
	})
}

func statusCmd(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlags("status")
	asJSON := fs.Bool("json", false, "")
	watch := fs.Bool("watch", false, "")
	if err := parse(fs, args); err != nil {
		return err
	}
	tables := fs.Args()
	for _, t := range tables {
		if !contains(status.Tables, t) {
			return usagef("thelb status: no table %q: the tables are %s", t, strings.Join(status.Tables, ", "))
		}
	}
	show := func() error {
		resp, err := localsock.Call(runDir, localsock.Request{Op: localsock.OpStatus})
		if err != nil {
			return err
		}
		if resp.Status == nil {
			return errors.New("theLB answered without a status")
		}
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(resp.Status)
		}
		text, err := resp.Status.Text(now(), tables...)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprint(stdout, text)
		return nil
	}
	if !*watch {
		return show()
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		_, _ = fmt.Fprintf(stdout, "-- %s --\n", now().UTC().Format(time.RFC3339))
		if err := show(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			_, _ = fmt.Fprintln(stdout)
		}
	}
}

func generateCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return usagef("thelb generate: say what to generate: secret (systemd, sysctl, modules, k8s and config come later)")
	}
	switch args[0] {
	case "secret":
		if len(args) > 1 {
			return usagef("thelb generate secret: unexpected %q", args[1])
		}
		s, err := secret.Generate()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stdout, s.Encode())
		return nil
	case "systemd", "sysctl", "modules", "k8s", "config":
		return notYet("thelb generate " + args[0])
	default:
		return usagef("thelb generate: %q is not something theLB generates", args[0])
	}
}

func adminCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return usagef("thelb admin: say what to change: approve, refuse, remove, secret show (pool, service, assign and unassign come later)")
	}
	switch args[0] {
	case "approve":
		return approveCmd(args[1:], stdout)
	case "refuse", "remove":
		fs := newFlags("admin " + args[0])
		if err := parse(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return usagef("thelb admin %s: name one node", args[0])
		}
		op := localsock.OpRefuse
		if args[0] == "remove" {
			op = localsock.OpRemove
		}
		resp, err := localsock.Call(runDir, localsock.Request{Op: op, Names: fs.Args()})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "%s: %s\n", map[string]string{"refuse": "refused", "remove": "removed"}[args[0]], strings.Join(resp.Names, ", "))
		return nil
	case "secret":
		if len(args) < 2 {
			return usagef("thelb admin secret: show (rotate and retire come later)")
		}
		switch args[1] {
		case "show":
			fs := newFlags("admin secret show")
			role := fs.String("role", "", "")
			if err := parse(fs, args[2:]); err != nil {
				return err
			}
			if *role != "director" && *role != "backend" {
				return usagef("thelb admin secret show: --role director or --role backend")
			}
			resp, err := localsock.Call(runDir, localsock.Request{Op: localsock.OpSecretShow, Role: *role})
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(stdout, resp.Secret)
			return nil
		case "rotate", "retire":
			return notYet("thelb admin secret " + args[1])
		default:
			return usagef("thelb admin secret: %q is not a secret command", args[1])
		}
	case "pool", "service", "assign", "unassign":
		return notYet("thelb admin " + args[0])
	default:
		return usagef("thelb admin: %q is not an admin command", args[0])
	}
}

// approveCmd approves pending nodes by name, or every one whose name matches
// --match - shown first, and approved after a "y", or at once with --yes.
func approveCmd(args []string, stdout io.Writer) error {
	fs := newFlags("admin approve")
	match := fs.String("match", "", "")
	yes := fs.Bool("yes", false, "")
	if err := parse(fs, args); err != nil {
		return err
	}
	names := fs.Args()
	switch {
	case *match == "" && len(names) == 0:
		return usagef("thelb admin approve: name the nodes to approve, or --match '<regular expression>'")
	case *match != "" && len(names) > 0:
		return usagef("thelb admin approve: names or --match, not both")
	case *yes && *match == "":
		return usagef("thelb admin approve: --yes goes with --match")
	}
	if *match != "" {
		re, err := regexp.Compile(`^(?:` + *match + `)$`)
		if err != nil {
			return usagef("thelb admin approve: --match %q: %v", *match, err)
		}
		resp, err := localsock.Call(runDir, localsock.Request{Op: localsock.OpStatus})
		if err != nil {
			return err
		}
		var matched status.Status
		for _, p := range resp.Status.Pending {
			if re.MatchString(p.Pending) {
				matched.Pending = append(matched.Pending, p)
				names = append(names, p.Pending)
			}
		}
		if len(names) == 0 {
			return fmt.Errorf("no pending node's name matches %q: nothing approved", *match)
		}
		text, err := matched.Text(now(), "pending")
		if err != nil {
			return err
		}
		_, _ = fmt.Fprint(stdout, text)
		if !*yes {
			if !isTerminal() {
				return errors.New("nothing approved: there is no terminal to confirm on - add --yes to approve these")
			}
			_, _ = fmt.Fprintf(stdout, "Approve %s? [y/N] ", plural(len(names), "this node", "these %d nodes"))
			line, _ := bufio.NewReader(stdin).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				return errors.New("nothing approved")
			}
		}
	}
	resp, err := localsock.Call(runDir, localsock.Request{Op: localsock.OpApprove, Names: names})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "approved: %s\n", strings.Join(resp.Names, ", "))
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(many, n)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
