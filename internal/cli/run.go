package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/dmartsapp/thelb/internal/backend"
	"github.com/dmartsapp/thelb/internal/check"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/ctlnet"
	"github.com/dmartsapp/thelb/internal/director"
	"github.com/dmartsapp/thelb/internal/hostnet"
	"github.com/dmartsapp/thelb/internal/kube"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/preflight"
	"github.com/dmartsapp/thelb/internal/safe"
)

// thelb director and thelb backend run a node. Their flags are defined once,
// in directorFlags and backendFlags, and checked once, into a director.Config
// or backend.Config: generate systemd and generate k8s director take the same
// flags - "the flags it runs with" - and check them the same way.

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// directorFlags are thelb director's flags.
type directorFlags struct {
	vip, secretFile, backendSecretFile string
	port                               int
	bootstrap                          bool
	bootstrapWait                      time.Duration
	cluster, name, iface               string
	metrics, kubeconfig                string
}

// add defines the flags on c, in their sections, the required ones marked.
func (f *directorFlags) add(c *cobra.Command) {
	fs := c.Flags()
	fs.StringVar(&f.vip, "vip", "", "The control VIP: an `address`, or a name resolved at start (required)")
	fs.IntVar(&f.port, "port", 0, "The control `port` (required)")
	fs.StringVar(&f.secretFile, "secret-file", "", "The directors' secret, an absolute `path` (required)")
	fs.StringVar(&f.backendSecretFile, "backend-secret-file", "", "The backends' secret, an absolute `path` (required)")
	fs.BoolVar(&f.bootstrap, "bootstrap", false, "May create the cluster, when none answers on the control VIP")
	fs.DurationVar(&f.bootstrapWait, "bootstrap-wait", 0, "With --bootstrap: how long to look for the cluster first, 5s to 10m (default 5s; 31s or more in a pod)")
	fs.StringVar(&f.cluster, "cluster", "thelb", "The cluster's `name`, when it creates it")
	fs.StringVar(&f.name, "name", "", "This node's `name` (default: its hostname)")
	fs.StringVar(&f.iface, "interface", "", "The control VIP's `interface` (default: the one in its network)")
	fs.StringVar(&f.kubeconfig, "kubeconfig", "", "Serve Kubernetes Services of class dmarts.app/thelb with this kubeconfig (an absolute `path`), or in-cluster for a pod's ServiceAccount")
	fs.StringVar(&f.metrics, "metrics", "", "Serve Prometheus metrics at /metrics on this `address:port` - an address of this machine, not 0.0.0.0")
	section(c, "Cluster", "vip", "port", "secret-file", "backend-secret-file", "bootstrap", "bootstrap-wait", "cluster")
	section(c, "Node", "name", "interface")
	section(c, "Kubernetes", "kubeconfig")
	section(c, "Monitoring", "metrics")
	required(c, "vip", "port", "secret-file", "backend-secret-file")
}

// config checks the flags, as cmd names the command in errors.
func (f *directorFlags) config(ctx context.Context, debug bool) (director.Config, error) {
	var cfg director.Config
	if f.iface != "" && !safe.IfName(f.iface) {
		return cfg, usagef("--interface %q is not an interface's name", f.iface)
	}
	if strings.Contains(f.vip, "/") {
		return cfg, usagef("--vip is the control VIP's address or name, without a prefix: %q", f.vip)
	}
	addr, vipName, err := vipAddr(ctx, "--vip", f.vip, f.iface, false)
	if err != nil {
		return cfg, err
	}
	cfg.ControlVIP, cfg.ControlVIPName = addr, vipName
	if f.port < 1 || f.port > 65535 {
		return cfg, usagef("--port, the control port, must be 1 to 65535")
	}
	cfg.ControlPort = uint16(f.port)
	for _, sf := range []struct{ flag, path string }{{"--secret-file", f.secretFile}, {"--backend-secret-file", f.backendSecretFile}} {
		if !filepath.IsAbs(sf.path) {
			return cfg, usagef("%s must be an absolute path: %q", sf.flag, sf.path)
		}
	}
	if filepath.Clean(f.secretFile) == filepath.Clean(f.backendSecretFile) {
		return cfg, usagef("the directors' and the backends' secrets need two files")
	}
	cfg.SecretFile, cfg.BackendSecretFile = filepath.Clean(f.secretFile), filepath.Clean(f.backendSecretFile)
	if !nameRE.MatchString(f.cluster) {
		return cfg, usagef("--cluster %q: a cluster's name is lowercase letters, digits and hyphens, up to 63", f.cluster)
	}
	cfg.Cluster = f.cluster
	if cfg.Name, err = nodeName(f.name); err != nil {
		return cfg, err
	}
	cfg.Bootstrap = f.bootstrap
	cfg.Interface = f.iface
	cfg.Debug = debug
	if cfg.Metrics, err = metricsAddr(f.metrics, "127.0.0.1:9732"); err != nil {
		return cfg, err
	}
	switch k := f.kubeconfig; {
	case k == "" || k == kube.InCluster:
		cfg.Kubeconfig = k
	case !filepath.IsAbs(k):
		return cfg, usagef("--kubeconfig is an absolute path, or %s for a pod's ServiceAccount: %q", kube.InCluster, k)
	default:
		cfg.Kubeconfig = filepath.Clean(k)
	}
	if f.bootstrapWait != 0 {
		if !f.bootstrap {
			return cfg, usagef("--bootstrap-wait is how long a director with --bootstrap looks for the cluster before creating one: give --bootstrap too")
		}
		if f.bootstrapWait < 5*time.Second || f.bootstrapWait > 10*time.Minute {
			return cfg, usagef("--bootstrap-wait is 5s to 10m, not %s", f.bootstrapWait)
		}
		cfg.BootstrapWait = f.bootstrapWait
	}
	return cfg, nil
}

// nodeName is --name, or this machine's hostname, checked as a node's name.
func nodeName(name string) (string, error) {
	if name == "" {
		h, err := os.Hostname()
		if err != nil {
			return "", fmt.Errorf("this node's hostname: %w; name it with --name", err)
		}
		name = strings.ToLower(strings.SplitN(h, ".", 2)[0])
	}
	if !nameRE.MatchString(name) {
		return "", usagef("the node's name %q: a node's name is lowercase letters, digits and hyphens, up to 63 - name it with --name", name)
	}
	return name, nil
}

// metricsAddr is --metrics, checked: an address of this machine and a port.
func metricsAddr(at, example string) (string, error) {
	if at == "" {
		return "", nil
	}
	if ap, err := netip.ParseAddrPort(at); err != nil || ap.Addr().IsUnspecified() {
		return "", usagef("--metrics is an address and port to serve metrics on, as %s - a machine's own address, not 0.0.0.0 or ::, which would include the VIPs: %q", example, at)
	}
	return at, nil
}

// given is the flags given to c - its own, in the order defined - and
// --debug, as a command line: what generate puts in a unit or a pod, so the
// node runs with them.
func given(c *cobra.Command, debug bool) []string {
	var out []string
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if !f.Changed {
			return
		}
		switch v := f.Value.(type) {
		case pflag.SliceValue:
			for _, s := range v.GetSlice() {
				out = append(out, "--"+f.Name, s)
			}
		default:
			if f.Value.Type() == "bool" {
				if f.Value.String() == "true" {
					out = append(out, "--"+f.Name)
				} else {
					out = append(out, "--"+f.Name+"=false")
				}
				return
			}
			out = append(out, "--"+f.Name, f.Value.String())
		}
	})
	if debug {
		out = append(out, "--debug")
	}
	return out
}

func (a *app) directorCmd() *cobra.Command {
	var f directorFlags
	c := &cobra.Command{
		Use:   "director [flags]",
		Short: "Run this node as a director",
		Long: `Run this node as a director. It looks for the cluster on the control VIP and
port, and joins it - then waits for a director's approval, unless the cluster
approved this node before. With --bootstrap, it creates the cluster when none
answers. The leader holds the control VIP; on SIGTERM it hands over first.

The first bootstrap writes the two secret files if they are missing (root
only, never overwritten). Everything else stays in memory.

With --kubeconfig, it serves Kubernetes Services too. Kubernetes never stops
a director: until the API server answers and allows it all, the director
serves everything else, says why, and asks again.`,
		Example: `  # The first director: creates the cluster and the secret files
  thelb director --vip 10.0.0.10 --port 7443 --bootstrap \
    --secret-file /etc/thelb/director.secret \
    --backend-secret-file /etc/thelb/backend.secret

  # Another director, with copies of the two files
  thelb director --vip 10.0.0.10 --port 7443 \
    --secret-file /etc/thelb/director.secret \
    --backend-secret-file /etc/thelb/backend.secret`,
		Annotations: map[string]string{annDocs: docsURL + "docs/directors.html"},
		Args:        noArgs,
	}
	f.add(c)
	c.RunE = a.act(func(_ *cobra.Command, _ []string) error {
		cfg, err := f.config(a.ctx, a.debug)
		if err != nil {
			return err
		}
		if goos != "linux" {
			return errors.New("thelb director runs only on Linux")
		}
		log := a.logger()
		log.SetDebug(cfg.Debug)
		return runDirector(a.ctx, cfg, director.Deps{
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
			Dataplane:   func() (director.Dataplane, error) { return dataplane(log.Module("dataplane")) },
			Preflight:   func() (preflight.Result, error) { return preflightDirector(cfg.ControlVIP) },
			ProbeWindow: cfg.BootstrapWait,
		})
	})
	return c
}

// logger is a running node's log: standard error, as systemd's journal takes it.
func (a *app) logger() *logx.Logger {
	out := logOutput
	if out == nil {
		out = a.stderr
	}
	return logx.New(out, "thelb")
}

// backendFlags are thelb backend's flags.
type backendFlags struct {
	join, secretFile string
	offers           []string
	name, iface      string
	metrics          string
}

func (f *backendFlags) add(c *cobra.Command) {
	fs := c.Flags()
	fs.StringVar(&f.join, "join", "", "The control VIP - an address or a name - and port, as `address:port` (required)")
	fs.StringVar(&f.secretFile, "secret-file", "", "The backends' secret, an absolute `path` (required)")
	fs.StringArrayVar(&f.offers, "offer", nil, "An application to offer, as often as needed - an `offer` like [name=]tcp/8080[,check=http:/healthz] - the protocol tcp, udp or sctp; the check tcp (TCP's default), http:<path>, or none (UDP's and SCTP's default)")
	fs.StringVar(&f.name, "name", "", "This node's `name` (default: its hostname)")
	fs.StringVar(&f.iface, "interface", "", "The control VIP's `interface` (default: the one in its network)")
	fs.StringVar(&f.metrics, "metrics", "", "Serve Prometheus metrics at /metrics on this `address:port`")
	section(c, "Cluster", "join", "secret-file", "offer")
	section(c, "Node", "name", "interface")
	section(c, "Monitoring", "metrics")
	required(c, "join", "secret-file")
}

func (f *backendFlags) config(ctx context.Context) (backend.Config, error) {
	var cfg backend.Config
	if f.iface != "" && !safe.IfName(f.iface) {
		return cfg, usagef("--interface %q is not an interface's name", f.iface)
	}
	host, portText, err := net.SplitHostPort(f.join)
	port, perr := strconv.Atoi(portText)
	if err != nil || perr != nil || port < 1 || port > 65535 {
		return cfg, usagef("--join is the control VIP, by address or name, and its port, as 10.0.0.10:7443, thelb-vip:7443 or [fd00::10]:7443: %q", f.join)
	}
	addr, joinName, err := vipAddr(ctx, "--join", host, f.iface, true)
	if err != nil {
		return cfg, err
	}
	cfg.Join, cfg.JoinName = netip.AddrPortFrom(addr, uint16(port)), joinName
	if !filepath.IsAbs(f.secretFile) {
		return cfg, usagef("--secret-file, the backends' secret, is an absolute path: %q", f.secretFile)
	}
	cfg.SecretFile = filepath.Clean(f.secretFile)
	seen := map[string]bool{}
	for _, spec := range f.offers {
		o, err := parseOffer(spec)
		if err != nil {
			return cfg, usagef("%v", err)
		}
		if seen[o.Key()] {
			return cfg, usagef("%s is offered twice", o.Key())
		}
		if seen["name "+o.Name] {
			return cfg, usagef("two offers are named %s", o.Name)
		}
		seen[o.Key()], seen["name "+o.Name] = true, true
		cfg.Offers = append(cfg.Offers, o)
	}
	if cfg.Name, err = nodeName(f.name); err != nil {
		return cfg, err
	}
	cfg.Interface = f.iface
	if cfg.Metrics, err = metricsAddr(f.metrics, "127.0.0.1:9733"); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (a *app) backendCmd() *cobra.Command {
	var f backendFlags
	c := &cobra.Command{
		Use:   "backend --join <address:port> --secret-file <path> [flags]",
		Short: "Run this node as a backend",
		Long: `Run this node as a backend. It joins on the control VIP with the backends'
secret, and waits for a director's approval. Then it offers its applications,
checks them, sends a heartbeat every 2 seconds, and holds the VIPs of the
services it serves on its own interface, thelb0. A director assigns its offers
to services; a backend never chooses one.

Where a service's port differs from its offer's, the backend maps it in its
own nftables table, inet thelb. On a director's host, start the backend after
the director, with a name of its own.

See also: thelb offer, thelb admin assign`,
		Example: `  thelb backend --join 10.0.0.10:7443 --secret-file /etc/thelb/backend.secret \
    --offer nginx=tcp/8080,check=http:/healthz --offer udp/53`,
		Annotations: map[string]string{annDocs: docsURL + "docs/backends.html"},
		Args:        noArgs,
	}
	f.add(c)
	c.RunE = a.act(func(_ *cobra.Command, _ []string) error {
		cfg, err := f.config(a.ctx)
		if err != nil {
			return err
		}
		if goos != "linux" {
			return errors.New("thelb backend runs only on Linux")
		}
		log := a.logger()
		log.SetDebug(a.debug)
		return runBackend(a.ctx, cfg, backend.Deps{
			RunDir:    roleDir("backend"),
			Log:       log,
			Now:       now,
			Host:      hostInputs,
			Kernel:    func() (backend.Kernel, error) { return backendKernel(log.Module("kernel")) },
			Preflight: func() (preflight.Result, error) { return preflightBackend(cfg.Join.Addr()) },
		})
	})
	return c
}

// noArgs is for a command that takes flags only.
func noArgs(c *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usagef("unexpected %q: %s takes flags only", args[0], c.CommandPath())
	}
	return nil
}

// nArgs is for a command that takes n arguments, as usage shows them.
func nArgs(n int, usage string) cobra.PositionalArgs {
	return func(c *cobra.Command, args []string) error {
		if len(args) != n {
			return usagef("%s takes %s", c.CommandPath(), usage)
		}
		return nil
	}
}

// roleDir is where a role's socket is: one director and one backend may run on
// one node, each once.
func roleDir(role string) string { return filepath.Join(runDir, role) }

// call asks the theLB running here in the role's directory, and says so with
// --debug.
func (a *app) call(role string, req localsock.Request) (localsock.Response, error) {
	start := time.Now()
	resp, err := localsock.Call(roleDir(role), req)
	switch {
	case err != nil:
		a.debugf("asked the %s at %s: %s - %v", role, filepath.Join(roleDir(role), localsock.SocketName), req.Op, err)
	default:
		a.debugf("asked the %s at %s: %s, answered in %s", role, filepath.Join(roleDir(role), localsock.SocketName), req.Op, time.Since(start).Round(time.Millisecond))
	}
	return resp, err
}

// callDirector asks the director running on this node.
func (a *app) callDirector(req localsock.Request) (localsock.Response, error) {
	resp, err := a.call("director", req)
	if errors.Is(err, localsock.ErrNotRunning) {
		if _, berr := a.call("backend", localsock.Request{Op: localsock.OpStatus}); berr == nil {
			return resp, errors.New("this node runs a backend, not a director: thelb admin runs on a director")
		}
		return resp, errors.New("no director runs on this node")
	}
	return resp, err
}

// callStatus asks for the status: of the role named, or of the director if one
// runs here, of the backend otherwise.
func (a *app) callStatus(role string, brief bool) (localsock.Response, error) {
	req := localsock.Request{Op: localsock.OpStatus, Brief: brief}
	if role != "" {
		return a.call(role, req)
	}
	resp, err := a.call("director", req)
	if errors.Is(err, localsock.ErrNotRunning) {
		return a.call("backend", req)
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

// parseOffer reads an offer: [name=]tcp/8080[,check=http:/healthz]. Without a
// name, it is named for its protocol and port: tcp-8080.
func parseOffer(spec string) (cluster.Offer, error) {
	main, opts, _ := strings.Cut(spec, ",")
	name := ""
	if n, rest, named := strings.Cut(main, "="); named {
		name, main = n, rest
		if !cluster.OfferNameOK(name) {
			return cluster.Offer{}, fmt.Errorf("an offer's name is letters, digits, dots, hyphens and underscores, up to 63, beginning and ending with a letter or a digit, not %q", name)
		}
	}
	proto, portText, ok := strings.Cut(main, "/")
	port, err := strconv.Atoi(portText)
	if !ok || err != nil || port < 1 || port > 65535 || (proto != "tcp" && proto != "udp" && proto != "sctp") {
		return cluster.Offer{}, fmt.Errorf("an offer is [<name>=]<tcp|udp|sctp>/<port>[,check=tcp|none|http:<path>], not %q", spec)
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
	if name == "" {
		name = cluster.DefaultOfferName(proto, port)
	}
	return cluster.Offer{Name: name, Proto: proto, Port: port, Check: chk}, nil
}
