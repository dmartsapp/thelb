package cli

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dmartsapp/thelb/internal/kube"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/safe"
	"github.com/dmartsapp/thelb/internal/secret"
)

// thelb generate prints files theLB needs - it writes nothing, to disk or to
// a cluster: the user keeps or installs what it prints.

func (a *app) generateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "generate <command>",
		Short: "Print a file theLB needs - it writes nothing",
		Long:  "Print a file theLB needs, for you to keep or install. It writes nothing.",
	}
	a.group(c)
	c.AddGroup(
		&cobra.Group{ID: "cluster", Title: "The cluster:"},
		&cobra.Group{ID: "boot", Title: "This node, from boot:"},
		&cobra.Group{ID: "k8s", Title: "Kubernetes:"},
	)
	secretCmd := &cobra.Command{
		Use:   "secret",
		Short: "A new cluster secret",
		Long: `Print a new cluster secret: for a secret file made before the first bootstrap.
The first bootstrap makes the files it lacks, so this is optional.`,
		Example: "  thelb generate secret > /etc/thelb/director.secret",
		Args:    noArgs,
		RunE: a.act(func(*cobra.Command, []string) error {
			s, err := secret.Generate()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(a.stdout, s.Encode())
			return nil
		}),
	}
	commandsCmd := &cobra.Command{
		Use:   "commands",
		Short: "The admin commands that create this cluster's setup again",
		Long: `On a director: print the thelb admin commands that create the cluster's pools,
services and assignments again, as they are now - a shell script to keep, and
to run on a cluster created again after every director stopped. theLB never
reads it.`,
		Example: "  thelb generate commands > /root/thelb-restore.sh",
		Args:    noArgs,
		RunE: a.act(func(*cobra.Command, []string) error {
			resp, err := a.callDirector(localsock.Request{Op: localsock.OpGenerate, What: "commands"})
			if err != nil {
				return err
			}
			_, _ = fmt.Fprint(a.stdout, resp.Text)
			return nil
		}),
	}
	addTo(c, "cluster", secretCmd, commandsCmd)
	addTo(c, "boot", a.systemdCmd(), a.bootCmd("sysctl"), a.bootCmd("modules"))
	addTo(c, "k8s", a.k8sCmd())
	return c
}

// systemdCmd is generate systemd director|backend: a unit that runs the role
// with the flags given - the role's own, checked as the role checks them.
func (a *app) systemdCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "systemd <director|backend>",
		Short: "A systemd unit for a director or a backend",
		Long: `Print a systemd unit that runs this binary as a director or a backend, with
the flags given - checked as the role checks them. The unit uses Type=notify,
the watchdog and Restart=always, and bounds theLB's capabilities and what it
may write.`,
		Example: `  thelb generate systemd director --vip 10.0.0.10 --port 7443 \
    --secret-file /etc/thelb/director.secret \
    --backend-secret-file /etc/thelb/backend.secret |
    sudo tee /etc/systemd/system/thelb-director.service
  sudo systemctl daemon-reload && sudo systemctl enable --now thelb-director`,
		Annotations: map[string]string{annDocs: docsURL + "docs/systemd.html"},
	}
	var df directorFlags
	dir := &cobra.Command{
		Use:   "director [flags]",
		Short: "A unit for a director, with the flags thelb director takes",
		Long: `Print a systemd unit that runs this binary as a director, with these flags -
the flags of thelb director, checked as it checks them.`,
		Example: `  thelb generate systemd director --vip 10.0.0.10 --port 7443 \
    --secret-file /etc/thelb/director.secret \
    --backend-secret-file /etc/thelb/backend.secret |
    sudo tee /etc/systemd/system/thelb-director.service`,
		Annotations: map[string]string{annDocs: docsURL + "docs/systemd.html"},
		Args:        noArgs,
	}
	df.add(dir)
	dir.RunE = a.act(func(c *cobra.Command, _ []string) error {
		cfg, err := df.config(a.ctx, a.debug)
		if err != nil {
			return err
		}
		paths := []string{cfg.SecretFile, cfg.BackendSecretFile} // files it reads
		if cfg.Kubeconfig != "" && cfg.Kubeconfig != kube.InCluster {
			paths = append(paths, cfg.Kubeconfig)
		}
		var writable []string // directories it may write
		if cfg.Bootstrap {    // the first bootstrap writes the secret files that are missing
			for _, p := range paths {
				if d := filepath.Dir(p); !slices.Contains(writable, d) {
					writable = append(writable, d)
				}
			}
		}
		return a.printUnit("director", given(c, a.debug), paths, writable, "30")
	})
	var bf backendFlags
	back := &cobra.Command{
		Use:   "backend --join <address:port> --secret-file <path> [flags]",
		Short: "A unit for a backend, with the flags thelb backend takes",
		Long: `Print a systemd unit that runs this binary as a backend, with these flags -
the flags of thelb backend, checked as it checks them.`,
		Example: `  thelb generate systemd backend --join 10.0.0.10:7443 \
    --secret-file /etc/thelb/backend.secret --offer nginx=tcp/8080 |
    sudo tee /etc/systemd/system/thelb-backend.service`,
		Annotations: map[string]string{annDocs: docsURL + "docs/systemd.html"},
		Args:        noArgs,
	}
	bf.add(back)
	back.RunE = a.act(func(c *cobra.Command, _ []string) error {
		cfg, err := bf.config(a.ctx)
		if err != nil {
			return err
		}
		return a.printUnit("backend", given(c, a.debug), []string{cfg.SecretFile}, nil, "1")
	})
	return a.group(c, dir, back)
}

// printUnit prints the unit that runs this binary in role, with flags.
func (a *app) printUnit(role string, flags, paths, writable []string, restart string) error {
	bin, err := executable()
	if err != nil {
		return fmt.Errorf("where this binary is: %w", err)
	}
	if b, err := filepath.EvalSymlinks(bin); err == nil {
		bin = b
	}
	if err := safe.Arg(bin); err != nil {
		return fmt.Errorf("this binary's path %v: a unit cannot hold it - move the binary to /usr/local/bin", err)
	}
	_, _ = fmt.Fprint(a.stdout, systemdUnit(role, bin, flags, paths, writable, restart))
	return nil
}

// systemdUnit is the unit: Type=notify and the watchdog, restarted always, and
// every hardening setting that does not stop theLB working - its capabilities
// bounded to what it uses, the system read-only but for what it writes. Not
// ProtectKernelTunables=: theLB writes /proc/sys/net; not ProtectKernelModules=:
// the kernel loads IPVS's, nftables' and the tunnels' modules on first use.
func systemdUnit(role, bin string, flags, paths, writable []string, restart string) string {
	under := func(dirs ...string) bool {
		for _, p := range append([]string{bin}, paths...) {
			for _, d := range dirs {
				if strings.HasPrefix(p, d) {
					return true
				}
			}
		}
		return false
	}
	exec := []string{systemdArg(bin), role}
	for _, f := range flags {
		exec = append(exec, systemdArg(f))
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# theLB %s, as `thelb generate systemd` printed it. Install it, and start it:", role)
	w("#   sudo tee /etc/systemd/system/thelb-%s.service <this file", role)
	w("#   sudo systemctl daemon-reload && sudo systemctl enable --now thelb-%s", role)
	if under("/tmp/", "/var/tmp/") {
		w("# The binary or a secret file is in /tmp, which does not survive a reboot:")
		w("# keep the binary in /usr/local/bin and the secret files in /etc/thelb.")
	}
	w("[Unit]")
	w("Description=theLB %s", role)
	w("Documentation=https://dmartsapp.github.io/thelb/docs/systemd.html")
	w("After=network-online.target")
	w("Wants=network-online.target")
	w("")
	w("[Service]")
	w("Type=notify")
	w("NotifyAccess=main")
	w("ExecStart=%s", strings.Join(exec, " "))
	w("Restart=always")
	if role == "director" {
		w("# Longer than the other of two directors takes to stand alone after a crash -")
		w("# the control VIP's lease (6 s unless set) and about 15 s more: back sooner,")
		w("# with --bootstrap it could create a cluster of its own first. With a longer")
		w("# lease, lengthen it: https://dmartsapp.github.io/thelb/docs/directors.html#two-directors")
	}
	w("RestartSec=%s", restart)
	w("WatchdogSec=30")
	w("TimeoutStopSec=30")
	w("")
	w("# What theLB needs - the network's administration, raw packets, a control port")
	w("# below 1024 if it is one - and no more.")
	w("CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE")
	w("NoNewPrivileges=yes")
	w("ProtectSystem=full")
	for _, d := range writable {
		w("ReadWritePaths=-%s", systemdPath(d))
	}
	if under("/home/", "/root/", "/run/user/") {
		w("# Read-only, not hidden: a file it needs is in a home directory.")
		w("ProtectHome=read-only")
	} else {
		w("ProtectHome=yes")
	}
	if !under("/tmp/", "/var/tmp/") {
		w("PrivateTmp=yes")
	}
	w("ProtectKernelLogs=yes")
	w("ProtectControlGroups=yes")
	w("ProtectClock=yes")
	w("ProtectHostname=yes")
	w("RestrictSUIDSGID=yes")
	w("RestrictRealtime=yes")
	w("RestrictNamespaces=yes")
	w("LockPersonality=yes")
	w("MemoryDenyWriteExecute=yes")
	w("SystemCallArchitectures=native")
	w("RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK AF_PACKET")
	w("")
	w("[Install]")
	w("WantedBy=multi-user.target")
	return b.String()
}

// systemdArg is an argument as ExecStart= reads it: specifiers (%) and
// variables ($) doubled, quoted where it has spaces, quotes or backslashes.
func systemdArg(a string) string {
	a = strings.NewReplacer("%", "%%", "$", "$$").Replace(a)
	if a != "" && !strings.ContainsAny(a, " \t\"'\\;") {
		return a
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a) + `"`
}

// systemdPath is a path as ReadWritePaths= reads it: specifiers (%) doubled,
// quoted where it has spaces, quotes or backslashes. No variables there: a "$"
// stays a "$".
func systemdPath(p string) string {
	p = strings.ReplaceAll(p, "%", "%%")
	if p != "" && !strings.ContainsAny(p, " \t\"'\\") {
		return p
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p) + `"`
}

// Kernel parameters and modules, by role: what theLB sets and loads while it
// runs, for a user who wants them from boot.
var (
	sysctls = map[string][]string{
		"director": {
			"# IPVS's - they exist once its module is loaded: thelb generate modules director.",
			"net.ipv4.vs.sloppy_tcp = 1",
			"net.ipv4.vs.expire_nodest_conn = 1",
			"net.ipv4.vs.expire_quiescent_template = 1",
		},
		"backend": {
			"# The VIPs on thelb0 answer no ARP, and are never announced from it.",
			"net.ipv4.conf.all.arp_ignore = 1",
			"net.ipv4.conf.all.arp_announce = 2",
			"# For tunnelled (IP-in-IP) services, where reverse-path filtering is strict (1):",
			"# net.ipv4.conf.all.rp_filter = 2",
		},
	}
	modules = map[string][]string{
		"director": {"ip_vs", "ip_vs_wrr", "ip_vs_rr", "ip_vs_wlc", "ip_vs_lc", "ip_vs_sh", "ip_vs_mh"},
		"backend": {
			"dummy",
			"# For tunnelled (IP-in-IP) services:",
			"# ipip",
			"# For port mapping:",
			"# nf_tables",
		},
	}
)

// bootCmd is generate sysctl or generate modules: a sysctl.d or
// modules-load.d file for a role, for what theLB sets while it runs to be set
// from boot.
func (a *app) bootCmd(what string) *cobra.Command {
	short, long, example := "A sysctl.d file with the kernel parameters theLB sets",
		`Print a sysctl.d file with the kernel parameters theLB sets while it runs, for
them from boot. theLB itself never writes one.`,
		"  thelb generate sysctl backend | sudo tee /etc/sysctl.d/90-thelb-backend.conf"
	if what == "modules" {
		short, long, example = "A modules-load.d file with the kernel modules theLB uses",
			`Print a modules-load.d file with the kernel modules theLB uses, for them from
boot.`,
			"  thelb generate modules director | sudo tee /etc/modules-load.d/thelb-director.conf"
	}
	return &cobra.Command{
		Use:       what + " <director|backend>",
		Short:     short,
		Long:      long,
		Example:   example,
		ValidArgs: []string{"director", "backend"},
		Args: func(c *cobra.Command, args []string) error {
			if len(args) != 1 || (args[0] != "director" && args[0] != "backend") {
				return usagef("%s takes a role: director or backend", c.CommandPath())
			}
			return nil
		},
		RunE: a.act(func(_ *cobra.Command, args []string) error {
			role := args[0]
			file, lines := "/etc/sysctl.d/90-thelb-"+role+".conf", sysctls[role]
			what2 := "the kernel parameters"
			if what == "modules" {
				file, lines, what2 = "/etc/modules-load.d/thelb-"+role+".conf", modules[role], "the kernel modules"
			}
			_, _ = fmt.Fprintf(a.stdout, "# theLB %s: %s it sets, or loads, while it runs - for them from boot.\n# Install: sudo thelb generate %s %s | sudo tee %s\n%s\n", role, what2, what, role, file, strings.Join(lines, "\n"))
			return nil
		}),
	}
}
