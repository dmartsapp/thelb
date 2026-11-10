package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dmartsapp/thelb/internal/doctor"
	"github.com/dmartsapp/thelb/internal/localsock"
)

// generateSystemd prints a systemd unit that runs a role with the flags given,
// checked as the role checks them: theLB prints it, the user installs it.
func generateSystemd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || (args[0] != "director" && args[0] != "backend") {
		return usagef("thelb generate systemd director|backend <the flags it runs with>: a unit that runs it so")
	}
	role, flags := args[0], args[1:]
	cmd := "generate systemd " + role
	var paths, writable []string // files it reads; directories it may write
	restart := "1"
	switch role {
	case "director":
		cfg, err := directorConfig(ctx, cmd, flags)
		if err != nil {
			return err
		}
		paths = []string{cfg.SecretFile, cfg.BackendSecretFile}
		if cfg.Bootstrap { // the first bootstrap writes the secret files that are missing
			for _, p := range paths {
				if d := filepath.Dir(p); !slices.Contains(writable, d) {
					writable = append(writable, d)
				}
			}
		}
		restart = "30"
	case "backend":
		cfg, err := backendConfig(ctx, cmd, flags)
		if err != nil {
			return err
		}
		paths = []string{cfg.SecretFile}
	}
	bin, err := executable()
	if err != nil {
		return fmt.Errorf("thelb %s: where this binary is: %w", cmd, err)
	}
	if b, err := filepath.EvalSymlinks(bin); err == nil {
		bin = b
	}
	_, _ = fmt.Fprint(stdout, systemdUnit(role, bin, flags, paths, writable, restart))
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
	w("Documentation=https://github.com/dmartsapp/thelb/blob/main/docs/manual.md")
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
		w("# lease, lengthen it (manual, section 5).")
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
		w("ReadWritePaths=-%s", systemdArg(d))
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

// generateBoot prints a sysctl.d or modules-load.d file for a role.
func generateBoot(what string, args []string, stdout io.Writer) error {
	if len(args) != 1 || (args[0] != "director" && args[0] != "backend") {
		return usagef("thelb generate %s director|backend", what)
	}
	role := args[0]
	file, lines := "/etc/sysctl.d/90-thelb-"+role+".conf", sysctls[role]
	what2 := "the kernel parameters"
	if what == "modules" {
		file, lines, what2 = "/etc/modules-load.d/thelb-"+role+".conf", modules[role], "the kernel modules"
	}
	_, _ = fmt.Fprintf(stdout, "# theLB %s: %s it sets, or loads, while it runs - for them from boot.\n# Install: sudo thelb generate %s %s | sudo tee %s\n%s\n", role, what2, what, role, file, strings.Join(lines, "\n"))
	return nil
}

// doctorCmd checks this node for theLB - for the roles running here, the one
// --role names, or both - and changes nothing. It fails (exit 1) when theLB
// could not run, or not do part of its work.
func doctorCmd(args []string, stdout io.Writer) error {
	fs := newFlags("doctor")
	role := fs.String("role", "", "")
	port := fs.Int("port", 0, "")
	vip := fs.String("vip", "", "")
	asJSON := fs.Bool("json", false, "")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("thelb doctor: unexpected %q", fs.Arg(0))
	}
	if *role != "" && *role != "director" && *role != "backend" {
		return usagef("thelb doctor: --role is director or backend, not %q", *role)
	}
	if *port < 0 || *port > 65535 {
		return usagef("thelb doctor: --port is the control port, 1 to 65535")
	}
	if _, err := netip.ParseAddr(*vip); *vip != "" && err != nil {
		return usagef("thelb doctor: --vip is the control VIP's address, not %q", *vip)
	}
	facts := doctor.Facts{Running: map[string]bool{}}
	if resp, err := localsock.Call(roleDir("director"), localsock.Request{Op: localsock.OpStatus}); err == nil && resp.Status != nil {
		facts.Running["director"], facts.Status = true, resp.Status
		for _, v := range resp.Status.VIPs {
			if v.Purpose == "control" {
				facts.Port, facts.Interface, facts.VIP = v.Port, v.Interface, v.VIP
			}
		}
	}
	if _, err := localsock.Call(roleDir("backend"), localsock.Request{Op: localsock.OpStatus}); err == nil {
		facts.Running["backend"] = true
	}
	switch {
	case *role != "":
		facts.Roles = []string{*role}
	case facts.Running["director"] || facts.Running["backend"]:
		for _, r := range []string{"director", "backend"} {
			if facts.Running[r] {
				facts.Roles = append(facts.Roles, r)
			}
		}
	default:
		facts.Roles = []string{"director", "backend"}
	}
	if *port > 0 {
		facts.Port = *port
	}
	if *vip != "" {
		facts.VIP = *vip
	}
	findings := doctor.Check(facts)
	if *asJSON {
		b, err := json.MarshalIndent(findings, "", "  ")
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stdout, string(b))
	} else {
		_, _ = fmt.Fprintf(stdout, "theLB doctor, for a %s on this node - it changes nothing:\n\n%s", strings.Join(facts.Roles, " and a "), doctor.Render(findings))
	}
	if doctor.Failed(findings) {
		return errors.New("thelb doctor: something theLB needs is missing here - see what to do, above")
	}
	return nil
}
