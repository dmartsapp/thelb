package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dmartsapp/thelb/internal/doctor"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/status"
)

// thelb status and thelb doctor look: at the cluster, as the theLB running
// here sees it, and at this node. Neither changes anything.

// roleFlag is --role: which of the theLBs running here to ask.
func roleFlag(c *cobra.Command, role *string, usage string) {
	c.Flags().StringVar(role, "role", "", usage)
	_ = c.RegisterFlagCompletionFunc("role", cobra.FixedCompletions([]cobra.Completion{"director", "backend"}, cobra.ShellCompDirectiveNoFileComp))
}

func checkRole(role string) error {
	if role != "" && role != "director" && role != "backend" {
		return usagef("--role is director or backend, not %q", role)
	}
	return nil
}

func (a *app) statusCmd() *cobra.Command {
	var asJSON, watch bool
	var role string
	c := &cobra.Command{
		Use:   "status [<view>|<table> ...] [flags]",
		Short: "Show the cluster, as this node sees it",
		Long: `Show the cluster, as the theLB running on this node sees it. Without a view, a
summary. On a backend, only that backend.

Views:
  directors    The directors, those waiting for approval, their certificates
  backends     The backends, those waiting, their offers, who serves what
  vips         The VIPs and who holds them, the services, the pools
  all          Everything

Tables:
  cluster, nodes, certificates, pending, pools, offers, services`,
		Example: `  thelb status
  thelb status backends
  thelb status --watch services offers`,
		Annotations: map[string]string{annDocs: docsURL + "docs/status.html"},
		ValidArgs:   status.Names,
		Args: func(_ *cobra.Command, args []string) error {
			for _, t := range args {
				if !slices.Contains(status.Names, t) {
					return usagef("no view or table %q: name one of %s", t, strings.Join(status.Names, ", "))
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "Print it as JSON (everything, without a view)")
	c.Flags().BoolVar(&watch, "watch", false, "Show it again every 2 seconds")
	roleFlag(c, &role, "Ask the `director` or the backend, where a node runs both (default: the director)")
	c.RunE = a.act(func(_ *cobra.Command, names []string) error {
		if err := checkRole(role); err != nil {
			return err
		}
		brief := !status.Counters(asJSON, names...)
		show := func() error {
			resp, err := a.callStatus(role, brief)
			if err != nil {
				return err
			}
			if resp.Status == nil {
				return errors.New("theLB answered without a status")
			}
			if asJSON {
				v, err := resp.Status.Pick(names...)
				if err != nil {
					return err
				}
				enc := json.NewEncoder(a.stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(v)
			}
			text, err := resp.Status.Text(now(), names...)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprint(a.stdout, text)
			return nil
		}
		if !watch {
			return show()
		}
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			_, _ = fmt.Fprintf(a.stdout, "-- %s --\n", now().UTC().Format(time.RFC3339))
			if err := show(); err != nil {
				return err
			}
			select {
			case <-a.ctx.Done():
				return nil
			case <-t.C:
				_, _ = fmt.Fprintln(a.stdout)
			}
		}
	})
	return c
}

// doctorCmd checks this node for theLB - for the roles running here, the one
// --role names, or both - and changes nothing. It fails (exit 1) when theLB
// could not run, or not do part of its work.
func (a *app) doctorCmd() *cobra.Command {
	var role, vip string
	var port int
	var asJSON bool
	c := &cobra.Command{
		Use:   "doctor [flags]",
		Short: "Check this node for theLB, and say what to fix",
		Long: `Check this node for theLB, and say what to fix. It changes nothing.

It checks the kernel, modules and IPv6; theLB's capabilities and kernel
parameters; systemd; the firewall, with the commands that open what theLB
needs; SELinux; software in the way (keepalived, kube-proxy, Cilium); what
listens on every address; the platform; the clock; the cluster as this node
sees it; and the link to the gateway and the other directors. Then it says
what theLB would add, set and start here.

The exit status is 1 when theLB could not run here, or not do all its work.`,
		Example: `  thelb doctor
  thelb doctor --role director --vip 10.0.0.10 --port 7443`,
		Annotations: map[string]string{annDocs: docsURL + "docs/troubleshooting.html"},
		Args:        noArgs,
	}
	roleFlag(c, &role, "Check for this `role`: director or backend (default: the roles running here, or both)")
	c.Flags().StringVar(&vip, "vip", "", "The control VIP's `address`, when no director runs here")
	c.Flags().IntVar(&port, "port", 0, "The control `port`, when no director runs here")
	c.Flags().BoolVar(&asJSON, "json", false, "Print the findings as JSON")
	c.RunE = a.act(func(_ *cobra.Command, _ []string) error {
		if err := checkRole(role); err != nil {
			return err
		}
		if port < 0 || port > 65535 {
			return usagef("--port is the control port, 1 to 65535")
		}
		if _, err := netip.ParseAddr(vip); vip != "" && err != nil {
			return usagef("--vip is the control VIP's address, not %q", vip)
		}
		facts := doctor.Facts{Running: map[string]bool{}}
		if resp, err := a.call("director", localsock.Request{Op: localsock.OpStatus}); err == nil && resp.Status != nil {
			facts.Running["director"], facts.Status = true, resp.Status
			for _, v := range resp.Status.VIPs {
				if v.Purpose == "control" {
					facts.Port, facts.Interface, facts.VIP = v.Port, v.Interface, v.VIP
				}
			}
		}
		if _, err := a.call("backend", localsock.Request{Op: localsock.OpStatus}); err == nil {
			facts.Running["backend"] = true
		}
		switch {
		case role != "":
			facts.Roles = []string{role}
		case facts.Running["director"] || facts.Running["backend"]:
			for _, r := range []string{"director", "backend"} {
				if facts.Running[r] {
					facts.Roles = append(facts.Roles, r)
				}
			}
		default:
			facts.Roles = []string{"director", "backend"}
		}
		if port > 0 {
			facts.Port = port
		}
		if vip != "" {
			facts.VIP = vip
		}
		findings := doctor.Check(facts)
		if asJSON {
			b, err := json.MarshalIndent(findings, "", "  ")
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(a.stdout, string(b))
		} else {
			_, _ = fmt.Fprintf(a.stdout, "theLB doctor, for a %s on this node - it changes nothing:\n\n%s", strings.Join(facts.Roles, " and a "), doctor.Render(findings))
		}
		if doctor.Failed(findings) {
			return errors.New("something theLB needs is missing here - see what to do, above")
		}
		return nil
	})
	return c
}
