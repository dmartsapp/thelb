package cli

import (
	"bufio"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/safe"
	"github.com/dmartsapp/thelb/internal/status"
)

// thelb admin changes the cluster: each subcommand asks the director running
// on this node, which sends the change through the leader.

// change asks the running director to change the cluster, and prints what it
// did.
func (a *app) change(c api.Change) error {
	resp, err := a.callDirector(localsock.Request{Op: localsock.OpChange, Change: &c})
	if err != nil {
		return err
	}
	if r := resp.Result; r != nil && r.Message != "" {
		_, _ = fmt.Fprintln(a.stdout, safe.Text(r.Message))
	}
	return nil
}

func (a *app) adminCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "admin <command>",
		Short: "Change the cluster (on a director)",
		Long: `Change the cluster. Runs on a director; a change made on another director goes
through the leader.`,
		Annotations: map[string]string{annDocs: docsURL + "docs/approval.html"},
	}
	a.group(c)
	c.AddGroup(
		&cobra.Group{ID: "members", Title: "Nodes:"},
		&cobra.Group{ID: "services", Title: "Services:"},
		&cobra.Group{ID: "cluster", Title: "The cluster:"},
	)
	addTo(c, "members", a.approveCmd(), a.nodeChangeCmd("refuse", "Refuse a node waiting to join",
		"Refuse a node waiting to join. It is told so, and stops."),
		a.nodeChangeCmd("remove", "Remove a node from the cluster",
			`Remove a node from the cluster: its certificate is revoked, a director leaves
the vote, a backend's assignments end. If it asks again, it waits for approval.`))
	addTo(c, "services", a.poolCmd(), a.serviceCmd(), a.assignCmd(), a.unassignCmd())
	addTo(c, "cluster", a.leaseCmd(), a.secretCmd())
	return c
}

// approveCmd approves pending nodes by name, or every one whose name matches
// --match - shown first, and approved after a "y", or at once with --yes.
func (a *app) approveCmd() *cobra.Command {
	var match string
	var yes bool
	c := &cobra.Command{
		Use:   "approve <name> ... | --match <regex> [--yes]",
		Short: "Approve nodes waiting to join",
		Long: `Approve nodes waiting to join. Compare their fingerprints first: thelb status
pending lists them.`,
		Example: `  thelb admin approve web-1 web-2
  thelb admin approve --match 'web-[0-9]+'`,
		Annotations: map[string]string{annDocs: docsURL + "docs/approval.html"},
	}
	c.Flags().StringVar(&match, "match", "", "Every waiting node whose whole name matches this `regex` (RE2 syntax), shown first, then approved after a \"y\"")
	c.Flags().BoolVar(&yes, "yes", false, "Approve the matches without asking, for scripts")
	c.Args = func(_ *cobra.Command, names []string) error {
		switch {
		case match == "" && len(names) == 0:
			return usagef("name the nodes to approve, or --match '<regular expression>'")
		case match != "" && len(names) > 0:
			return usagef("names or --match, not both")
		case yes && match == "":
			return usagef("--yes goes with --match")
		}
		return nil
	}
	c.RunE = a.act(func(_ *cobra.Command, names []string) error {
		if match != "" {
			re, err := regexp.Compile(`^(?:` + match + `)$`)
			if err != nil {
				return usagef("--match %q: %v", match, err)
			}
			resp, err := a.callDirector(localsock.Request{Op: localsock.OpStatus})
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
				return fmt.Errorf("no pending node's name matches %q: nothing approved", match)
			}
			text, err := matched.Text(now(), "pending")
			if err != nil {
				return err
			}
			_, _ = fmt.Fprint(a.stdout, text)
			if !yes {
				if !isTerminal() {
					return errors.New("nothing approved: there is no terminal to confirm on - add --yes to approve these")
				}
				_, _ = fmt.Fprintf(a.stdout, "Approve %s? [y/N] ", plural(len(names), "this node", "these %d nodes"))
				line, _ := bufio.NewReader(stdin).ReadString('\n')
				if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
					return errors.New("nothing approved")
				}
			}
		}
		resp, err := a.callDirector(localsock.Request{Op: localsock.OpChange, Change: &api.Change{Op: "approve", Names: names}})
		if err != nil {
			return err
		}
		done := slices.Clone(names)
		if resp.Result != nil && len(resp.Result.Names) > 0 {
			done = slices.Clone(resp.Result.Names)
		}
		slices.Sort(done) // by name, not by the order they asked in
		_, _ = fmt.Fprintf(a.stdout, "approved: %s\n", safe.Text(strings.Join(done, ", ")))
		return nil
	})
	return c
}

// nodeChangeCmd is admin refuse or admin remove: one node, by name.
func (a *app) nodeChangeCmd(op, short, long string) *cobra.Command {
	c := &cobra.Command{
		Use:   op + " <name>",
		Short: short,
		Long:  long,
		Args:  nArgs(1, "one node's name: "+op+" <name>"),
	}
	c.RunE = a.act(func(_ *cobra.Command, args []string) error {
		resp, err := a.callDirector(localsock.Request{Op: localsock.OpChange, Change: &api.Change{Op: op, Names: args}})
		if err != nil {
			return err
		}
		done := args
		if resp.Result != nil && len(resp.Result.Names) > 0 {
			done = resp.Result.Names
		}
		_, _ = fmt.Fprintf(a.stdout, "%s: %s\n", map[string]string{"refuse": "refused", "remove": "removed"}[op], safe.Text(strings.Join(done, ", ")))
		return nil
	})
	return c
}

func (a *app) poolCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "pool <command>",
		Short: "Add or remove a pool of service VIPs",
		Long:  "Manage the pools that services and Kubernetes Services take their VIPs from.",
	}
	var vip, iface string
	var count int
	var def bool
	add := &cobra.Command{
		Use:   "add <name> --vip <first address/prefix> --vippool-count <n> [flags]",
		Short: "Add a pool",
		Long: `Add a pool of addresses for service VIPs, on the director's interface in that
network.`,
		Example: `  # 10.10.1.20 to 10.10.1.29
  thelb admin pool add main --vip 10.10.1.20/24 --vippool-count 10 --default`,
		Args: nArgs(1, "the pool's name: pool add <name> --vip <first address/prefix> --vippool-count <n>"),
	}
	add.Flags().StringVar(&vip, "vip", "", "The first address, and its network's prefix, as `address/prefix` (required)")
	add.Flags().IntVar(&count, "vippool-count", 0, "How many addresses: `n`, 1 to 4096 (required)")
	add.Flags().StringVar(&iface, "interface", "", "The `interface` (default: the one in that network)")
	add.Flags().BoolVar(&def, "default", false, "The pool a service takes its VIP from when it names none - one for each address family")
	required(add, "vip", "vippool-count")
	add.RunE = a.act(func(_ *cobra.Command, args []string) error {
		if _, err := netip.ParsePrefix(vip); err != nil {
			return usagef("--vip is the pool's first address and its network's prefix, as 10.10.1.20/24: %q", vip)
		}
		if count < 1 {
			return usagef("--vippool-count, how many addresses, is 1 to 4096")
		}
		if iface != "" && !safe.IfName(iface) {
			return usagef("--interface %q is not an interface's name", iface)
		}
		return a.change(api.Change{Op: "pool.add", Pool: args[0], VIP: vip, Count: count, Interface: iface, Default: def})
	})
	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a pool no service uses",
		Long:  "Remove a pool that no service, and no Kubernetes Service, uses.",
		Args:  nArgs(1, "the pool's name: pool remove <name>"),
	}
	remove.RunE = a.act(func(_ *cobra.Command, args []string) error {
		return a.change(api.Change{Op: "pool.remove", Pool: args[0]})
	})
	return a.group(c, add, remove)
}

func (a *app) serviceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "service <command>",
		Short: "Add or remove a service",
		Long:  "Manage services: a VIP, a protocol and a port, served by backends' offers.",
	}
	var pool, vip, proto, sched, method string
	var port int
	add := &cobra.Command{
		Use:   "add <name> --proto <tcp|udp|sctp> --port <port> [flags]",
		Short: "Add a service",
		Long: `Add a service. Its VIP is the lowest free address of its pool, or the one
asked for - probed on the network first. It comes up once a backend serves it.`,
		Example: "  thelb admin service add web --proto tcp --port 80",
		Args:    nArgs(1, "the service's name: service add <name> --proto <tcp|udp|sctp> --port <port>"),
	}
	fs := add.Flags()
	fs.StringVar(&proto, "proto", "", "The protocol, `proto`: tcp, udp or sctp (required)")
	fs.IntVar(&port, "port", 0, "The `port` (required)")
	fs.StringVar(&pool, "pool", "", "Take the VIP from this `pool` (default: the default pool)")
	fs.StringVar(&vip, "vip", "", "This `address` of the pool")
	fs.StringVar(&sched, "scheduler", "", "The IPVS `scheduler` - how it picks a backend: wrr (default), rr, wlc, lc, sh or mh")
	fs.StringVar(&method, "method", "", "The `method`: direct - backends on the director's network (default); tunnel - IP-in-IP, backends anywhere routed (IPv4)")
	required(add, "proto", "port")
	_ = add.RegisterFlagCompletionFunc("proto", cobra.FixedCompletions([]cobra.Completion{"tcp", "udp", "sctp"}, cobra.ShellCompDirectiveNoFileComp))
	_ = add.RegisterFlagCompletionFunc("scheduler", cobra.FixedCompletions([]cobra.Completion{"wrr", "rr", "wlc", "lc", "sh", "mh"}, cobra.ShellCompDirectiveNoFileComp))
	_ = add.RegisterFlagCompletionFunc("method", cobra.FixedCompletions([]cobra.Completion{"direct", "tunnel"}, cobra.ShellCompDirectiveNoFileComp))
	add.RunE = a.act(func(_ *cobra.Command, args []string) error {
		return a.change(api.Change{Op: "service.add", Names: args, Pool: pool, VIP: vip, Proto: proto, Port: port, Scheduler: sched, Method: method})
	})
	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a service",
		Long:  "Remove a service: its VIP, its IPVS service and its assignments.",
		Args:  nArgs(1, "the service's name: service remove <name>"),
	}
	remove.RunE = a.act(func(_ *cobra.Command, args []string) error {
		return a.change(api.Change{Op: "service.remove", Names: args})
	})
	return a.group(c, add, remove)
}

func (a *app) assignCmd() *cobra.Command {
	var weights map[string]int
	c := &cobra.Command{
		Use:   "assign <service> <node>:<offer> ... [--weight <node>=<n>,...]",
		Short: "Assign backends' offers to a service",
		Long: `Assign backends' offers to a service. Name each offer by the name its backend
gave it, or by its protocol and port: thelb status offers lists them. The
offer's protocol must be the service's; where its port differs, the backend
maps the service's port to it.`,
		Example: `  thelb admin assign web web-1:nginx web-2:tcp/8080
  thelb admin assign web web-1:nginx web-2:nginx --weight web-1=3`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) < 2 {
				return usagef("assign takes a service and its offers: assign <service> <node>:<offer> ... - the offer by its name, or as <proto>/<port>")
			}
			return nil
		},
	}
	c.Flags().StringToIntVar(&weights, "weight", nil, "Weights, as `node=n,...` (default 1 each)")
	c.RunE = a.act(func(_ *cobra.Command, args []string) error {
		return a.change(api.Change{Op: "assign", Names: args[:1], Offers: args[1:], Weights: weights})
	})
	return c
}

func (a *app) unassignCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "unassign <service> <node>",
		Short: "Take an offer back from a service",
		Long:  "Take a backend's offer back from a service.",
		Args:  nArgs(2, "a service and a node: unassign <service> <node>"),
	}
	c.RunE = a.act(func(_ *cobra.Command, args []string) error {
		return a.change(api.Change{Op: "unassign", Names: args})
	})
	return c
}

func (a *app) leaseCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "lease <seconds>",
		Short: "Set how long the control VIP outlives its leader",
		Long: `Set how long the kernel keeps the control VIP after the leader's last renewal:
3 to 3600 seconds (default 6). A leader that dies or hangs loses the VIP
within it, and a director taking over waits it out. thelb status cluster shows
it.`,
		Example: "  thelb admin lease 10",
		Args:    nArgs(1, "a number of seconds: lease <seconds>, how long the kernel keeps the control VIP after the leader's last renewal"),
	}
	c.RunE = a.act(func(_ *cobra.Command, args []string) error {
		d, err := time.ParseDuration(args[0])
		if err != nil {
			n, aerr := strconv.Atoi(args[0])
			if aerr != nil {
				return usagef("%q is not a number of seconds", args[0])
			}
			d = time.Duration(n) * time.Second
		}
		if d%time.Second != 0 || d < cluster.MinLease || d > cluster.MaxLease {
			return usagef("whole seconds, %s to %s, not %s", cluster.MinLease, cluster.MaxLease, args[0])
		}
		return a.change(api.Change{Op: "lease", Seconds: int(d / time.Second)})
	})
	return c
}

func (a *app) secretCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "secret <command>",
		Short: "Show a role's secret",
		Long: `Show a role's secret, for a node whose secret file is out of date - the one
way to see a secret. Rotating secrets comes later.`,
	}
	var role string
	show := &cobra.Command{
		Use:     "show --role <director|backend>",
		Short:   "Print a role's secret",
		Long:    "Print a role's secret, for a node whose secret file is out of date.",
		Example: "  thelb admin secret show --role backend > /etc/thelb/backend.secret",
		Args:    noArgs,
	}
	roleFlag(show, &role, "The `role` whose secret to print: director or backend (required)")
	required(show, "role")
	show.RunE = a.act(func(_ *cobra.Command, _ []string) error {
		if role != "director" && role != "backend" {
			return usagef("--role is director or backend, not %q", role)
		}
		resp, err := a.callDirector(localsock.Request{Op: localsock.OpSecretShow, Role: role})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(a.stdout, resp.Secret)
		return nil
	})
	later := func(op string) *cobra.Command {
		return &cobra.Command{Use: op, Hidden: true, Args: noArgs,
			RunE: a.act(func(*cobra.Command, []string) error { return notYet("thelb admin secret " + op) })}
	}
	return a.group(c, show, later("rotate"), later("retire"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(many, n)
}
