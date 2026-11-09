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
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/backend"
	"github.com/dmartsapp/thelb/internal/check"
	"github.com/dmartsapp/thelb/internal/cluster"
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
	runDir      = "/run/thelb" // each role has its own directory in it: director/, backend/
	lookup      = net.DefaultResolver.LookupNetIP
	localFor    = hostnet.For
	now         = time.Now
	hostInputs  = fingerprint.ReadHost
	runDirector = director.Run
	runBackend  = backend.Run
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
		return runBackendCmd(ctx, rest, stderr)
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
		return usagef("thelb director: --vip is the control VIP's address or name, without a prefix: %q", *vip)
	}
	addr, vipName, err := vipAddr(ctx, "--vip", *vip, *iface, false)
	if err != nil {
		var u usageError
		if errors.As(err, &u) {
			return usagef("thelb director: %v", err)
		}
		return fmt.Errorf("thelb director: %w", err)
	}
	cfg.ControlVIP, cfg.ControlVIPName = addr, vipName
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
		RunDir: roleDir("director"),
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
		Dataplane: func() (director.Dataplane, error) { return dataplane(log.Module("dataplane")) },
	})
}

// roleDir is where a role's socket is: one director and one backend may run on
// one node, each once.
func roleDir(role string) string { return filepath.Join(runDir, role) }

// callDirector asks the director running on this node.
func callDirector(req localsock.Request) (localsock.Response, error) {
	resp, err := localsock.Call(roleDir("director"), req)
	if errors.Is(err, localsock.ErrNotRunning) {
		if _, berr := localsock.Call(roleDir("backend"), localsock.Request{Op: localsock.OpStatus}); berr == nil {
			return resp, errors.New("this node runs a backend, not a director: thelb admin runs on a director")
		}
		return resp, errors.New("no director runs on this node")
	}
	return resp, err
}

// callStatus asks for the status: of the role named, or of the director if one
// runs here, of the backend otherwise.
func callStatus(role string) (localsock.Response, error) {
	req := localsock.Request{Op: localsock.OpStatus}
	if role != "" {
		return localsock.Call(roleDir(role), req)
	}
	resp, err := localsock.Call(roleDir("director"), req)
	if errors.Is(err, localsock.ErrNotRunning) {
		return localsock.Call(roleDir("backend"), req)
	}
	return resp, err
}

var hostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*\.?$`)

// vipAddr is a --vip or --join host: an address as it is, or a name, resolved
// now - at start - to the one address it has in a network of this node.
func vipAddr(ctx context.Context, flag, host, iface string, routed bool) (netip.Addr, string, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap(), "", nil
	}
	if !hostRE.MatchString(host) {
		return netip.Addr{}, "", usagef("%s %q is neither an address nor a name", flag, host)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	addrs, err := lookup(ctx, "ip", host)
	if err != nil {
		return netip.Addr{}, "", fmt.Errorf("%s %s: the name does not resolve: %w", flag, host, err)
	}
	var in []netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if _, err := localFor(a, iface); err == nil && !slices.Contains(in, a) {
			in = append(in, a)
		}
	}
	if len(in) == 0 && routed && len(addrs) == 1 {
		return addrs[0].Unmap(), host, nil // behind a router: a backend of tunnelled services
	}
	switch len(in) {
	case 1:
		return in[0], host, nil
	case 0:
		return netip.Addr{}, "", fmt.Errorf("%s %s resolves to %v, and none of them is in a network of this node", flag, host, addrs)
	default:
		return netip.Addr{}, "", fmt.Errorf("%s %s resolves to more than one address in this node's networks, %v: name one address", flag, host, in)
	}
}

// offerFlags collects --offer, which may be given more than once.
type offerFlags []string

func (o *offerFlags) String() string     { return strings.Join(*o, " ") }
func (o *offerFlags) Set(v string) error { *o = append(*o, v); return nil }

// parseOffer reads an offer: tcp/8080, or tcp/8080,check=http:/healthz.
func parseOffer(spec string) (cluster.Offer, error) {
	main, opts, _ := strings.Cut(spec, ",")
	proto, portText, ok := strings.Cut(main, "/")
	port, err := strconv.Atoi(portText)
	if !ok || err != nil || port < 1 || port > 65535 || (proto != "tcp" && proto != "udp" && proto != "sctp") {
		return cluster.Offer{}, fmt.Errorf("an offer is <tcp|udp|sctp>/<port>[,check=tcp|none|http:<path>], not %q", spec)
	}
	chk := ""
	if opts != "" {
		v, ok := strings.CutPrefix(opts, "check=")
		if !ok {
			return cluster.Offer{}, fmt.Errorf("an offer's option is check=..., not %q", opts)
		}
		chk = v
	}
	if chk, err = check.Parse(chk, proto); err != nil {
		return cluster.Offer{}, fmt.Errorf("--offer %s: %w", spec, err)
	}
	return cluster.Offer{Proto: proto, Port: port, Check: chk}, nil
}

func runBackendCmd(ctx context.Context, args []string, stderr io.Writer) error {
	fs := newFlags("backend")
	joinTo := fs.String("join", "", "")
	secretFile := fs.String("secret-file", "", "")
	name := fs.String("name", "", "")
	iface := fs.String("interface", "", "")
	var offers offerFlags
	fs.Var(&offers, "offer", "")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("thelb backend: unexpected %q", fs.Arg(0))
	}
	var cfg backend.Config
	host, portText, err := net.SplitHostPort(*joinTo)
	port, perr := strconv.Atoi(portText)
	if err != nil || perr != nil || port < 1 || port > 65535 {
		return usagef("thelb backend: --join is the control VIP, by address or name, and its port, as 10.0.0.10:7443, thelb-vip:7443 or [fd00::10]:7443: %q", *joinTo)
	}
	addr, joinName, err := vipAddr(ctx, "--join", host, *iface, true)
	if err != nil {
		var u usageError
		if errors.As(err, &u) {
			return usagef("thelb backend: %v", err)
		}
		return fmt.Errorf("thelb backend: %w", err)
	}
	cfg.Join, cfg.JoinName = netip.AddrPortFrom(addr, uint16(port)), joinName
	if *secretFile == "" || !filepath.IsAbs(*secretFile) {
		return usagef("thelb backend: --secret-file, the backends' secret, is required, as an absolute path")
	}
	cfg.SecretFile = filepath.Clean(*secretFile)
	seen := map[string]bool{}
	for _, spec := range offers {
		o, err := parseOffer(spec)
		if err != nil {
			return usagef("thelb backend: %v", err)
		}
		if seen[o.Key()] {
			return usagef("thelb backend: %s is offered twice", o.Key())
		}
		seen[o.Key()] = true
		cfg.Offers = append(cfg.Offers, o)
	}
	cfg.Name = *name
	if cfg.Name == "" {
		h, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("thelb backend: this node's hostname: %w; name it with --name", err)
		}
		cfg.Name = strings.ToLower(strings.SplitN(h, ".", 2)[0])
	}
	if !nameRE.MatchString(cfg.Name) {
		return usagef("thelb backend: the node's name %q: a node's name is lowercase letters, digits and hyphens, up to 63 - name it with --name", cfg.Name)
	}
	cfg.Interface = *iface
	if goos != "linux" {
		return errors.New("thelb backend runs only on Linux")
	}
	out := logOutput
	if out == nil {
		out = stderr
	}
	log := logx.New(out, "thelb")
	return runBackend(ctx, cfg, backend.Deps{
		RunDir: roleDir("backend"),
		Log:    log,
		Now:    now,
		Host:   hostInputs,
		Kernel: func() (backend.Kernel, error) { return backendKernel(log.Module("kernel")) },
	})
}

func statusCmd(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlags("status")
	asJSON := fs.Bool("json", false, "")
	watch := fs.Bool("watch", false, "")
	role := fs.String("role", "", "")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *role != "" && *role != "director" && *role != "backend" {
		return usagef("thelb status: --role is director or backend")
	}
	names := fs.Args()
	for _, t := range names {
		if !contains(status.Names, t) {
			return usagef("thelb status: no view or table %q: name one of %s", t, strings.Join(status.Names, ", "))
		}
	}
	show := func() error {
		resp, err := callStatus(*role)
		if err != nil {
			return err
		}
		if resp.Status == nil {
			return errors.New("theLB answered without a status")
		}
		if *asJSON {
			v, err := resp.Status.Pick(names...)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(v)
		}
		text, err := resp.Status.Text(now(), names...)
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
		return usagef("thelb generate: say what to generate: secret or commands (systemd, sysctl, modules and k8s come later)")
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
	case "commands":
		if len(args) > 1 {
			return usagef("thelb generate commands: unexpected %q", args[1])
		}
		resp, err := callDirector(localsock.Request{Op: localsock.OpGenerate, What: "commands"})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprint(stdout, resp.Text)
		return nil
	case "systemd", "sysctl", "modules", "k8s":
		return notYet("thelb generate " + args[0])
	default:
		return usagef("thelb generate: %q is not something theLB generates", args[0])
	}
}

// change asks the running director to change the cluster, and prints what it
// did.
func change(stdout io.Writer, c api.Change) error {
	resp, err := callDirector(localsock.Request{Op: localsock.OpChange, Change: &c})
	if err != nil {
		return err
	}
	if r := resp.Result; r != nil && r.Message != "" {
		_, _ = fmt.Fprintln(stdout, r.Message)
	}
	return nil
}

func adminCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return usagef("thelb admin: say what to change: approve, refuse, remove, pool, service, assign, unassign, secret show")
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
		resp, err := callDirector(localsock.Request{Op: localsock.OpChange, Change: &api.Change{Op: args[0], Names: fs.Args()}})
		if err != nil {
			return err
		}
		done := fs.Args()
		if resp.Result != nil && len(resp.Result.Names) > 0 {
			done = resp.Result.Names
		}
		_, _ = fmt.Fprintf(stdout, "%s: %s\n", map[string]string{"refuse": "refused", "remove": "removed"}[args[0]], strings.Join(done, ", "))
		return nil
	case "pool":
		return poolCmd(args[1:], stdout)
	case "service":
		return serviceCmd(args[1:], stdout)
	case "assign":
		fs := newFlags("admin assign")
		weights := fs.String("weight", "", "")
		// The service and offers come first, the flags after them, as in the help.
		pos, flags := splitFlags(args[1:])
		if err := parse(fs, flags); err != nil {
			return err
		}
		pos = append(pos, fs.Args()...)
		if len(pos) < 2 {
			return usagef("thelb admin assign <service> <node>:<proto>/<port> ... [--weight node=N,...]")
		}
		w := map[string]int{}
		if *weights != "" {
			for _, kv := range strings.Split(*weights, ",") {
				k, v, ok := strings.Cut(kv, "=")
				n, err := strconv.Atoi(v)
				if !ok || err != nil {
					return usagef("thelb admin assign: --weight is node=N[,node=N], not %q", *weights)
				}
				w[k] = n
			}
		}
		return change(stdout, api.Change{Op: "assign", Names: pos[:1], Offers: pos[1:], Weights: w})
	case "unassign":
		if len(args) != 3 {
			return usagef("thelb admin unassign <service> <node>")
		}
		return change(stdout, api.Change{Op: "unassign", Names: args[1:]})
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
			resp, err := callDirector(localsock.Request{Op: localsock.OpSecretShow, Role: *role})
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
	default:
		return usagef("thelb admin: %q is not an admin command", args[0])
	}
}

// splitFlags separates leading arguments from the flags after them.
func splitFlags(args []string) (pos, flags []string) {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			return args[:i], args[i:]
		}
	}
	return args, nil
}

func poolCmd(args []string, stdout io.Writer) error {
	if len(args) < 2 || (args[0] != "add" && args[0] != "remove") {
		return usagef("thelb admin pool add <name> --vip <first address/prefix> --vippool-count <n> [--interface <name>] [--default], or pool remove <name>")
	}
	name := args[1]
	if args[0] == "remove" {
		if len(args) != 2 {
			return usagef("thelb admin pool remove <name>")
		}
		return change(stdout, api.Change{Op: "pool.remove", Pool: name})
	}
	fs := newFlags("admin pool add")
	vip := fs.String("vip", "", "")
	count := fs.Int("vippool-count", 0, "")
	iface := fs.String("interface", "", "")
	def := fs.Bool("default", false, "")
	if err := parse(fs, args[2:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("thelb admin pool add: unexpected %q", fs.Arg(0))
	}
	if _, err := netip.ParsePrefix(*vip); err != nil {
		return usagef("thelb admin pool add: --vip is the pool's first address and its network's prefix, as 10.10.1.20/24: %q", *vip)
	}
	if *count < 1 {
		return usagef("thelb admin pool add: --vippool-count, how many addresses, is required")
	}
	return change(stdout, api.Change{Op: "pool.add", Pool: name, VIP: *vip, Count: *count, Interface: *iface, Default: *def})
}

func serviceCmd(args []string, stdout io.Writer) error {
	if len(args) < 2 || (args[0] != "add" && args[0] != "remove") {
		return usagef("thelb admin service add <name> --proto tcp|udp|sctp --port <port> [--pool <name>] [--vip <address>] [--scheduler wrr], or service remove <name>")
	}
	name := args[1]
	if args[0] == "remove" {
		if len(args) != 2 {
			return usagef("thelb admin service remove <name>")
		}
		return change(stdout, api.Change{Op: "service.remove", Names: []string{name}})
	}
	fs := newFlags("admin service add")
	pool := fs.String("pool", "", "")
	vip := fs.String("vip", "", "")
	proto := fs.String("proto", "", "")
	port := fs.Int("port", 0, "")
	sched := fs.String("scheduler", "", "")
	method := fs.String("method", "", "")
	if err := parse(fs, args[2:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("thelb admin service add: unexpected %q", fs.Arg(0))
	}
	if *proto == "" || *port == 0 {
		return usagef("thelb admin service add: --proto and --port are required")
	}
	return change(stdout, api.Change{Op: "service.add", Names: []string{name}, Pool: *pool, VIP: *vip, Proto: *proto, Port: *port, Scheduler: *sched, Method: *method})
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
		resp, err := callDirector(localsock.Request{Op: localsock.OpStatus})
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
	resp, err := callDirector(localsock.Request{Op: localsock.OpChange, Change: &api.Change{Op: "approve", Names: names}})
	if err != nil {
		return err
	}
	done := names
	if resp.Result != nil && len(resp.Result.Names) > 0 {
		done = resp.Result.Names
	}
	_, _ = fmt.Fprintf(stdout, "approved: %s\n", strings.Join(done, ", "))
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
