//go:build linux

package doctor

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dmartsapp/thelb/internal/announce"
	"github.com/dmartsapp/thelb/internal/ctlnet"
	"github.com/dmartsapp/thelb/internal/hostnet"
	"github.com/dmartsapp/thelb/internal/ipvs"
	"github.com/dmartsapp/thelb/internal/netlink"
)

// Check checks the node for the roles in f, and the theLB running on it.
func Check(f Facts) []Finding {
	var out []Finding
	add := func(fs ...Finding) { out = append(out, fs...) }
	add(kernel(f)...)
	add(capabilities()...)
	add(parameters(f)...)
	add(running(f)...)
	add(firewall(f)...)
	add(selinux()...)
	add(inTheWay(f)...)
	if slices.Contains(f.Roles, "director") {
		add(listeners(f)...)
	}
	add(platform()...)
	add(clock()...)
	add(cluster(f)...)
	add(link(f)...)
	add(plan(f)...)
	return out
}

func has(f Facts, role string) bool { return slices.Contains(f.Roles, role) }

func release() string {
	var u syscall.Utsname
	if syscall.Uname(&u) != nil {
		return ""
	}
	var b []byte
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// module is whether a kernel module is loaded, built in, available to load,
// or missing.
func module(name string) string {
	if _, err := os.Stat("/sys/module/" + name); err == nil {
		return "loaded"
	}
	dir := "/lib/modules/" + release()
	if b, err := os.ReadFile(dir + "/modules.builtin"); err == nil && strings.Contains(string(b), "/"+name+".ko") {
		return "built in"
	}
	if b, err := os.ReadFile(dir + "/modules.dep"); err == nil && strings.Contains(string(b), "/"+name+".ko") {
		return "available"
	}
	return "missing"
}

func kernel(f Facts) []Finding {
	out := []Finding{{Area: "kernel", Check: "version", State: Info, Detail: release()}}
	// The kernel reports an address's protocol only for an address that has
	// one: the version says whether it has the field.
	marks := Finding{Area: "kernel", Check: "address protocol (IFA_PROTO)", State: OK, Detail: "Linux 6.1 or later: theLB marks its addresses by their protocol, 116, and IPv4 ones by their label too"}
	var major, minor int
	if _, err := fmt.Sscanf(release(), "%d.%d", &major, &minor); err == nil && (major < 6 || major == 6 && minor < 1) {
		marks.State, marks.Detail = Info, "before Linux 6.1, unless backported: theLB marks IPv4 addresses by their label, and knows IPv6 VIPs by their address"
	}
	out = append(out, marks)
	if _, err := os.Stat("/proc/sys/net/ipv6"); err != nil {
		out = append(out, Finding{Area: "kernel", Check: "IPv6", State: Info, Detail: "disabled (ipv6.disable=1?): IPv6 VIPs cannot be used"})
	} else {
		out = append(out, Finding{Area: "kernel", Check: "IPv6", State: OK, Detail: "enabled"})
	}
	if has(f, "director") {
		fd := Finding{Area: "kernel", Check: "SCTP in IPVS", State: OK, Detail: "built in: SCTP services can be served"}
		if _, err := os.Stat("/proc/sys/net/ipv4/vs"); err != nil {
			fd.State, fd.Detail = Info, "IPVS not loaded yet: known once it is"
		} else if _, err := os.Stat("/proc/sys/net/ipv4/vs/sloppy_sctp"); err != nil {
			fd.State, fd.Detail = Warn, "IPVS here was built without it (CONFIG_IP_VS_PROTO_SCTP): this director, leading, refuses SCTP services"
		}
		out = append(out, fd)
	}
	type need struct {
		name, role, why string
		required        bool
	}
	for _, m := range []need{
		{"ip_vs", "director", "IPVS, the data plane", true},
		{"ip_vs_wrr", "director", "the wrr scheduler, the default", true},
		{"ip_vs_mh", "director", "the mh (Maglev) scheduler, if a service uses it", false},
		{"dummy", "backend", "thelb0, where a backend holds its VIPs", true},
		{"ipip", "backend", "thelbt0, for tunnelled services", false},
		{"nf_tables", "backend", "port mapping", false},
	} {
		if !has(f, m.role) {
			continue
		}
		st := module(m.name)
		fd := Finding{Area: "kernel", Check: "module " + m.name, State: OK, Detail: st + " - " + m.why}
		switch {
		case st == "available":
			fd.Detail = "not loaded yet; the kernel loads it on first use - " + m.why
		case st == "missing" && m.required:
			fd.State, fd.Detail, fd.Fix = Fail, "not in this kernel's modules - "+m.why, "install the kernel's extra modules (linux-modules-extra-$(uname -r), kernel-modules-extra)"
		case st == "missing":
			fd.State, fd.Detail = Warn, "not in this kernel's modules - "+m.why+" cannot be used"
		}
		out = append(out, fd)
	}
	return out
}

func capabilities() []Finding {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil
	}
	var eff uint64
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "CapEff:"); ok {
			eff, _ = strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		}
	}
	fd := Finding{Area: "authority", Check: "capabilities", State: OK, Detail: "CAP_NET_ADMIN and CAP_NET_RAW"}
	if eff&(1<<12) == 0 || eff&(1<<13) == 0 {
		fd.State, fd.Detail, fd.Fix = Fail, "CAP_NET_ADMIN or CAP_NET_RAW missing here", "run theLB as root; under systemd, with both in CapabilityBoundingSet= (thelb generate systemd)"
	}
	return []Finding{fd}
}

// What theLB sets while it runs, by role.
var setting = map[string][][2]string{
	"director": {{"net.ipv4.vs.sloppy_tcp", "1"}, {"net.ipv4.vs.expire_nodest_conn", "1"}, {"net.ipv4.vs.expire_quiescent_template", "1"}},
	"backend":  {{"net.ipv4.conf.all.arp_ignore", "1"}, {"net.ipv4.conf.all.arp_announce", "2"}},
}

func parameters(f Facts) []Finding {
	var out []Finding
	boot := bootSysctls()
	if fh, err := os.OpenFile("/proc/sys/net/ipv4/conf/all/arp_ignore", os.O_WRONLY, 0); err != nil {
		out = append(out, Finding{Area: "parameters", Check: "/proc/sys/net", State: Fail, Detail: "cannot be set here: " + err.Error(), Fix: "theLB sets kernel parameters while it runs: not under ProtectKernelTunables=, nor in a container with /proc/sys read-only"})
	} else {
		_ = fh.Close()
	}
	for _, role := range f.Roles {
		for _, p := range setting[role] {
			now := "-"
			if b, err := os.ReadFile("/proc/sys/" + strings.ReplaceAll(p[0], ".", "/")); err == nil {
				now = strings.TrimSpace(string(b))
			} else if strings.HasPrefix(p[0], "net.ipv4.vs.") {
				now = "- (IPVS not loaded)"
			}
			from := "not set"
			if b, ok := boot[p[0]]; ok {
				from = b.value + " (" + b.file + ")"
			}
			fd := Finding{Area: "parameters", Check: p[0], State: Info, Detail: fmt.Sprintf("theLB sets %s while it runs; now %s; from boot: %s", p[1], now, from)}
			if !f.Running[role] && now != p[1] && from == "not set" {
				fd.Detail += " - set at theLB's start, restored when it stops"
			}
			out = append(out, fd)
		}
	}
	return out
}

type bootValue struct{ value, file string }

// bootSysctls is what systemd-sysctl sets at boot: the sysctl.d files - one of
// each name, the first directory's - in the order of their names, then
// /etc/sysctl.conf.
func bootSysctls() map[string]bootValue {
	files := map[string]string{}
	for _, d := range []string{"/etc/sysctl.d", "/run/sysctl.d", "/usr/local/lib/sysctl.d", "/usr/lib/sysctl.d", "/lib/sysctl.d"} {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if _, seen := files[e.Name()]; !seen && strings.HasSuffix(e.Name(), ".conf") {
				files[e.Name()] = filepath.Join(d, e.Name())
			}
		}
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	paths := make([]string, 0, len(names)+1)
	for _, n := range names {
		paths = append(paths, files[n])
	}
	paths = append(paths, "/etc/sysctl.conf")
	out := map[string]bootValue{}
	for _, p := range paths {
		fh, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || line[0] == '#' || line[0] == ';' {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k = strings.ReplaceAll(strings.TrimPrefix(strings.TrimSpace(k), "-"), "/", ".")
			out[k] = bootValue{strings.TrimSpace(v), p}
		}
		_ = fh.Close()
	}
	return out
}

// processes is the processes whose name is comm, by PID.
func processes(comm string) []int {
	var out []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if b, err := os.ReadFile("/proc/" + e.Name() + "/comm"); err == nil && strings.TrimSpace(string(b)) == comm {
			out = append(out, pid)
		}
	}
	return out
}

func command(name string, args ...string) (string, bool) {
	if _, err := exec.LookPath(name); err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(b), err == nil
}

// running says how this node's theLB runs: under systemd, with a RestartSec=
// long enough, or by hand.
func running(f Facts) []Finding {
	pids := processes("thelb")
	var out []Finding
	if len(pids) == 0 {
		return []Finding{{Area: "theLB", Check: "running here", State: Info, Detail: "no theLB runs on this node"}}
	}
	for _, pid := range pids {
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		args := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
		role := ""
		if len(args) > 1 {
			role = args[1]
		}
		if role != "director" && role != "backend" {
			continue
		}
		cg, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		unit := ""
		for _, part := range strings.Split(strings.TrimSpace(string(cg)), "/") {
			if strings.HasSuffix(part, ".service") {
				unit = part
			}
		}
		fd := Finding{Area: "theLB", Check: "the " + role, State: OK, Detail: fmt.Sprintf("process %d, under systemd: %s", pid, unit)}
		if unit == "" || strings.HasPrefix(unit, "user@") || strings.HasPrefix(unit, "session-") {
			fd.State, fd.Detail = Warn, fmt.Sprintf("process %d, started by hand: a crash, a reboot or a closed terminal stops it", pid)
			fd.Fix = "run it under systemd: thelb generate systemd " + role + " <its flags> prints the unit"
		} else if role == "director" {
			if v, ok := command("systemctl", "show", "-p", "RestartUSec", "--value", unit); ok {
				if d, err := time.ParseDuration(strings.TrimSpace(strings.ReplaceAll(v, "min", "m"))); err == nil && d < 30*time.Second {
					fd.State = Warn
					fd.Detail += fmt.Sprintf(", restarted after %s", d)
					fd.Fix = "RestartSec=30 or more in " + unit + ": restarted sooner, a crashed director of two can create a cluster of its own before the other stands alone"
				}
			}
		}
		out = append(out, fd)
	}
	return out
}

// network is the control VIP's interface's network, for firewall rules.
func network(f Facts) (string, string) {
	ifname := f.Interface
	if ifname == "" {
		// The interface of the default route: where an address beyond this
		// node's networks would go.
		if r, err := hostnet.Route(netip.MustParseAddr("192.0.2.1")); err == nil {
			ifname = r.Interface
		}
	}
	if ifname == "" {
		return "", ""
	}
	ifc, err := net.InterfaceByName(ifname)
	if err != nil {
		return ifname, ""
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return ifname, (&net.IPNet{IP: n.IP.Mask(n.Mask), Mask: n.Mask}).String()
		}
	}
	return ifname, ""
}

func firewall(f Facts) []Finding {
	ifname, nw := network(f)
	port := "<the control port>"
	known := f.Port > 0
	if known {
		port = strconv.Itoa(f.Port)
	}
	from := nw
	if from == "" {
		from = "<the directors' and backends' network>"
	}
	needs := "the control port (TCP " + port + ") from the directors and backends"
	if has(f, "director") {
		needs += ", and connection sync (UDP 8848 to 224.0.0.81) between directors"
	}
	if has(f, "backend") {
		needs += "; on a backend, its services' ports from the clients and its offers' ports from the directors"
	}
	unknown := ""
	if !known && has(f, "director") {
		unknown = " (thelb doctor --port <port> prints the rules with yours)"
	}
	if out, ok := command("ufw", "status"); ok && strings.Contains(out, "Status: active") {
		fd := Finding{Area: "firewall", Check: "ufw", State: Warn, Detail: "active: it must allow " + needs}
		if known && strings.Contains(out, port) {
			fd.State, fd.Detail = OK, "active, with a rule for port "+port+" - check that it allows "+needs
		}
		fix := fmt.Sprintf("sudo ufw allow from %s to any port %s proto tcp comment 'thelb control'", from, port)
		if has(f, "director") && ifname != "" {
			fix += fmt.Sprintf("; sudo ufw allow in on %s from %s to 224.0.0.81 port 8848 proto udp comment 'thelb connection sync'", ifname, from)
		}
		fd.Fix = fix + unknown
		return []Finding{fd}
	}
	if out, ok := command("firewall-cmd", "--state"); ok && strings.Contains(out, "running") {
		fd := Finding{Area: "firewall", Check: "firewalld", State: Warn, Detail: "running: it must allow " + needs}
		if ports, ok := command("firewall-cmd", "--list-ports"); ok && known && strings.Contains(ports, port+"/tcp") {
			fd.State, fd.Detail = OK, "running, with "+port+"/tcp open"
		}
		fix := fmt.Sprintf("sudo firewall-cmd --permanent --add-port=%s/tcp", port)
		if has(f, "director") {
			fix += ` && sudo firewall-cmd --permanent --add-rich-rule='rule family="ipv4" destination address="224.0.0.81" port port="8848" protocol="udp" accept'`
		}
		fd.Fix = fix + " && sudo firewall-cmd --reload" + unknown
		return []Finding{fd}
	}
	if out, ok := command("nft", "list", "ruleset"); ok {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "hook input") && strings.Contains(line, "policy drop") {
				return []Finding{{Area: "firewall", Check: "nftables", State: Warn, Detail: "an input chain drops by default: it must allow " + needs, Fix: "add rules accepting them to that chain, before its policy"}}
			}
		}
	}
	return []Finding{{Area: "firewall", Check: "firewall", State: OK, Detail: "none found dropping by default (ufw, firewalld, nftables)"}}
}

func selinux() []Finding {
	b, err := os.ReadFile("/sys/fs/selinux/enforce")
	if err != nil {
		return nil
	}
	mode := "permissive"
	if strings.TrimSpace(string(b)) == "1" {
		mode = "enforcing"
	}
	return []Finding{{Area: "platform", Check: "SELinux", State: Info, Detail: mode}}
}

// inTheWay is other software that would fight theLB for VIPs, ARP or IPVS.
func inTheWay(f Facts) []Finding {
	var out []Finding
	for _, p := range []struct{ comm, why string }{
		{"keepalived", "keepalived moves VIPs and programs IPVS too: the two would undo each other's work"},
		{"kube-proxy", "kube-proxy's rules catch VIP traffic before IPVS does: a director must not run it (design: Kubernetes)"},
		{"cilium-agent", "Cilium's kube-proxy replacement catches VIP traffic before IPVS does on a director"},
		{"kube-router", "kube-router programs IPVS too"},
	} {
		if len(processes(p.comm)) > 0 && (has(f, "director") || p.comm == "keepalived") {
			out = append(out, Finding{Area: "in the way", Check: p.comm, State: Warn, Detail: "running: " + p.why, Fix: "stop it on this node, or run theLB elsewhere"})
		}
	}
	if has(f, "director") && module("ip_vs") == "loaded" {
		ours := map[string]bool{}
		if f.Status != nil {
			for _, s := range f.Status.Services {
				ours[s.VIP] = true
			}
		}
		if c, err := ipvs.Open(); err == nil {
			if svcs, err := c.Services(); err == nil {
				var others []string
				for _, s := range svcs {
					if !ours[s.Addr.String()] {
						others = append(others, s.Key())
					}
				}
				if len(others) > 0 {
					out = append(out, Finding{Area: "in the way", Check: "other IPVS services", State: Info, Detail: fmt.Sprintf("%d not theLB's (%s): theLB leaves them alone", len(others), strings.Join(others[:min(3, len(others))], ", "))})
				}
			}
		}
	}
	if len(out) == 0 {
		out = append(out, Finding{Area: "in the way", Check: "others", State: OK, Detail: "no keepalived, kube-proxy, Cilium or kube-router here"})
	}
	return out
}

// listeners is what listens on every address of a director: it answers on
// every VIP the director holds.
func listeners(f Facts) []Finding {
	ports := map[int]bool{}
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[3] != "0A" { // LISTEN
				continue
			}
			addr, port, ok := strings.Cut(fields[1], ":")
			if !ok || strings.Trim(addr, "0") != "" {
				continue
			}
			if p, err := strconv.ParseUint(port, 16, 16); err == nil && int(p) != f.Port {
				ports[int(p)] = true
			}
		}
	}
	if len(ports) == 0 {
		return []Finding{{Area: "network", Check: "listeners on every address", State: OK, Detail: "none: nothing of this machine answers on the VIPs"}}
	}
	var list []string
	for p := range ports {
		list = append(list, strconv.Itoa(p))
	}
	sort.Slice(list, func(i, j int) bool { a, _ := strconv.Atoi(list[i]); b, _ := strconv.Atoi(list[j]); return a < b })
	return []Finding{{Area: "network", Check: "listeners on every address", State: Warn, Detail: "TCP " + strings.Join(list, ", ") + ": they answer on every VIP this director holds", Fix: "bind them to this machine's own address (ListenAddress for sshd), or filter them on the VIPs"}}
}

func dmi(name string) string {
	b, _ := os.ReadFile("/sys/class/dmi/id/" + name)
	return strings.TrimSpace(string(b))
}

// platform is where this machine runs: a cloud where a VIP cannot move by ARP,
// or a hypervisor that may need a setting for it.
func platform() []Finding {
	vendor, product, asset, bios := dmi("sys_vendor"), dmi("product_name"), dmi("chassis_asset_tag"), dmi("bios_vendor")
	cloud := ""
	switch {
	case strings.Contains(vendor, "Amazon") || strings.Contains(bios, "Amazon") || strings.HasPrefix(product, "EC2"):
		cloud = "AWS"
	case strings.Contains(vendor, "Google") || strings.Contains(product, "Google Compute Engine"):
		cloud = "Google Cloud"
	case vendor == "Microsoft Corporation" && asset == "7783-7084-3265-9085-8269-3286-77":
		cloud = "Azure"
	case strings.Contains(vendor, "OracleCloud") || strings.Contains(asset, "OracleCloud"):
		cloud = "Oracle Cloud"
	case strings.Contains(vendor, "DigitalOcean"), strings.Contains(vendor, "Hetzner"), strings.Contains(vendor, "Alibaba"), strings.Contains(vendor, "Scaleway"):
		cloud = vendor
	}
	if cloud != "" {
		return []Finding{{Area: "platform", Check: "cloud", State: Warn, Detail: cloud + ": its network does not let an address move between machines by ARP - VIPs taken over are not reached", Fix: "use the cloud's own load balancer or floating addresses; theLB does not drive them yet"}}
	}
	hv := ""
	switch {
	case strings.Contains(vendor, "VMware"):
		hv, cloud = "VMware", "allow MAC address changes and forged transmits on the port group"
	case strings.Contains(product, "OpenStack") || strings.Contains(vendor, "OpenStack"):
		hv, cloud = "OpenStack", "add the VIPs to the ports' allowed address pairs, or turn port security off"
	case vendor == "Microsoft Corporation" && product == "Virtual Machine":
		hv, cloud = "Hyper-V", "turn on MAC address spoofing on the network adapter"
	case strings.Contains(vendor, "QEMU") || strings.Contains(product, "KVM") || strings.Contains(product, "Standard PC"):
		hv = "KVM/QEMU"
	case strings.Contains(product, "VirtualBox"):
		hv = "VirtualBox"
	}
	if hv == "" {
		return []Finding{{Area: "platform", Check: "machine", State: Info, Detail: strings.TrimSpace(vendor + " " + product)}}
	}
	fd := Finding{Area: "platform", Check: "hypervisor", State: Info, Detail: hv}
	if cloud != "" {
		fd.Detail += ": VIPs move between machines only if it lets them - " + cloud
	}
	return []Finding{fd}
}

func clock() []Finding {
	v, ok := command("timedatectl", "show", "-p", "NTPSynchronized", "--value")
	if !ok {
		return nil
	}
	if strings.TrimSpace(v) == "yes" {
		return []Finding{{Area: "platform", Check: "clock", State: OK, Detail: "synchronised"}}
	}
	return []Finding{{Area: "platform", Check: "clock", State: Warn, Detail: "not synchronised: certificates last 24 hours, and directors compare their clocks' view of them", Fix: "turn on time synchronisation: sudo timedatectl set-ntp true, or chrony"}}
}

// cluster is the cluster as this node's director sees it.
func cluster(f Facts) []Finding {
	st := f.Status
	if st == nil || st.Cluster == nil {
		return nil
	}
	voters := 0
	for _, n := range st.Nodes {
		if (n.Role == "director" || n.Role == "leader") && (n.State == "up" || n.State == "catching up") {
			voters++
		}
	}
	out := []Finding{{Area: "cluster", Check: "cluster", State: Info, Detail: fmt.Sprintf("%s, led by %s", st.Cluster.Name, st.Cluster.Leader)}}
	fd := Finding{Area: "cluster", Check: "directors up", State: OK, Detail: strconv.Itoa(voters)}
	switch voters {
	case 1:
		fd.State, fd.Detail, fd.Fix = Warn, "1: no failover", "add directors: three survive any one's crash"
	case 2:
		fd.State, fd.Detail, fd.Fix = Warn, "2: either one's crash leaves the other to stand alone, after the control VIP's lease; a cut neither sees can make two leaders until it heals", "a third director: three keep their majority through any one's crash, with no waiting and no guess"
	}
	out = append(out, fd)
	if st.Cluster.Lease > 0 {
		out = append(out, Finding{Area: "cluster", Check: "control VIP lease", State: Info, Detail: fmt.Sprintf("%ds: a stopped leader's VIPs answer up to this long", st.Cluster.Lease)})
	}
	if ifc, err := net.InterfaceByName(f.Interface); err == nil && ifc.Flags&net.FlagMulticast == 0 {
		out = append(out, Finding{Area: "cluster", Check: "multicast", State: Warn, Detail: f.Interface + " has no multicast: connection sync cannot run, and a failover loses connections in progress", Fix: "an interface with multicast for the control VIP"})
	}
	return out
}

// self is this node's director in the status: the node whose address is one
// of this machine's.
func self(st *Facts) (name string) {
	if st.Status == nil {
		return ""
	}
	addrs, _ := net.InterfaceAddrs()
	mine := map[string]bool{}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			mine[n.IP.String()] = true
		}
	}
	for _, n := range st.Status.Nodes {
		if (n.Role == "director" || n.Role == "leader") && mine[n.Address] {
			return n.Node
		}
	}
	return ""
}

// link tests what theLB relies on from the link, sending nothing that changes
// anything: whether the gateway and the other directors answer ARP probes - a
// takeover reads a director's silence as its machine down; whether connection
// sync runs, and entries arrive; and, on a follower, whether its cache points
// the control VIP at the leader - the leader's announcements arrive.
func link(f Facts) []Finding {
	if !has(f, "director") {
		return nil
	}
	ifname, _ := network(f)
	ifc, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil
	}
	var out []Finding
	src := netip.Addr{}
	if addrs, err := ifc.Addrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap().Is4() {
					src = ip.Unmap()
					break
				}
			}
		}
	}
	probe := func(ip netip.Addr) string {
		who, err := announce.Probe(ifc.Index, ifc.HardwareAddr, src, ip, time.Second)
		if err != nil {
			return ""
		}
		return who
	}
	if gw, ok := ctlnet.Gateway(ifname, true); ok {
		fd := Finding{Area: "link", Check: "the gateway answers ARP", State: OK, Detail: gw.String() + " answers: a director cut off from it knows it, and does not take over"}
		if probe(gw) == "" {
			fd.State, fd.Detail, fd.Fix = Warn, gw.String()+" does not answer ARP probes: a director of two decides it is cut off, and never stands alone", "let the gateway answer ARP (probes from 0.0.0.0, RFC 5227), or remove the route through it from the control VIP's interface"
		}
		out = append(out, fd)
	}
	me := self(&f)
	var leaderMAC string
	if f.Status != nil {
		for _, n := range f.Status.Nodes {
			if n.Role == "leader" {
				leaderMAC = n.MAC
			}
			if (n.Role != "director" && n.Role != "leader") || n.Node == me || n.State != "up" {
				continue
			}
			ip, err := netip.ParseAddr(n.Address)
			if err != nil {
				continue
			}
			fd := Finding{Area: "link", Check: "director " + n.Node + " answers ARP", State: OK, Detail: n.Address + " answers probes: its silence would mean its machine is down"}
			if who := probe(ip); who == "" {
				fd.State, fd.Detail, fd.Fix = Warn, n.Address+" is up, but does not answer ARP probes: a takeover would read that as its machine down", "let ARP probes (from 0.0.0.0, RFC 5227) between the directors through - a switch's or a hypervisor's ARP filtering, or arp_ignore on "+n.Node
			}
			out = append(out, fd)
		}
	}
	if module("ip_vs") == "loaded" {
		var ds []ipvs.Daemon
		if c, err := ipvs.Open(); err == nil {
			ds, _ = c.Daemons()
		}
		switch {
		case len(ds) > 0:
			var roles []string
			for _, d := range ds {
				roles = append(roles, d.Role()+" on "+d.Interface)
			}
			fd := Finding{Area: "link", Check: "connection sync", State: OK, Detail: strings.Join(roles, ", ")}
			if b, err := os.ReadFile("/proc/net/ip_vs_conn"); err == nil {
				n := max(strings.Count(string(b), "\n")-1, 0)
				fd.Detail += fmt.Sprintf("; %d connection entries here", n)
				if n == 0 && me != "" && f.Status != nil && f.Status.Cluster != nil && f.Status.Cluster.Leader != me {
					fd.Detail += " - none yet; if the leader has connections in progress, multicast to 224.0.0.81 does not pass"
				}
			}
			out = append(out, fd)
		case f.Running["director"]:
			out = append(out, Finding{Area: "link", Check: "connection sync", State: Warn, Detail: "no IPVS sync daemon runs here: a failover loses connections in progress", Fix: "theLB starts them while it is a member: its log says why it could not"})
		}
	}
	if f.VIP != "" && leaderMAC != "" && me != "" && f.Status.Cluster.Leader != me {
		if vip, err := netip.ParseAddr(f.VIP); err == nil {
			e, ok, err := netlink.Neighbour(ifc.Index, vip)
			fd := Finding{Area: "link", Check: "gratuitous ARP", State: Info, Detail: "this node has no entry for the control VIP yet: nothing here has asked for it"}
			switch {
			case err != nil || !ok || len(e.MAC) == 0:
			case strings.EqualFold(e.MAC.String(), leaderMAC):
				fd.State, fd.Detail = OK, "this node's cache points the control VIP at the leader ("+leaderMAC+"): its announcements arrive"
			default:
				fd.State, fd.Detail, fd.Fix = Warn, "this node's cache points the control VIP at "+e.MAC.String()+", not the leader's "+leaderMAC+": its announcements did not arrive here, or another machine answers for the VIP", "a switch or hypervisor that drops gratuitous ARP: let it through; or find the other machine (thelb doctor on the leader)"
			}
			out = append(out, fd)
		}
	}
	return out
}

// plan is what theLB would change on this node, in its role: what director or
// backend adds, sets and starts - all of it runtime state, removed or restored
// when theLB stops.
func plan(f Facts) []Finding {
	var out []Finding
	lease := "6s"
	if f.Status != nil && f.Status.Cluster != nil && f.Status.Cluster.Lease > 0 {
		lease = fmt.Sprintf("%ds", f.Status.Cluster.Lease)
	}
	set := func(role string) {
		for _, p := range setting[role] {
			now := ""
			if b, err := os.ReadFile("/proc/sys/" + strings.ReplaceAll(p[0], ".", "/")); err == nil {
				now = strings.TrimSpace(string(b))
			}
			d := fmt.Sprintf("sets %s, restored when theLB stops (now %s)", p[1], now)
			switch now {
			case p[1]:
				d = "leaves it: already " + p[1]
			case "":
				d = "sets " + p[1] + " once its module is loaded, restored when theLB stops"
			}
			out = append(out, Finding{Area: "plan", Check: p[0], State: Info, Detail: d})
		}
	}
	if has(f, "director") {
		ifname, _ := network(f)
		vip := "the control VIP"
		if f.VIP != "" {
			vip = f.VIP
		}
		label := "label <interface>:thelb"
		if ifname != "" {
			label = "label " + ifname + ":thelb"
		}
		out = append(out, Finding{Area: "plan", Check: "control VIP", State: Info, Detail: fmt.Sprintf("while it leads: %s on %s - %s, protocol 116, leased %s, renewed every third of it - announced by gratuitous ARP", vip, ifname, label, lease)})
		svcs := "while it leads: each service VIP on its pool's interface, leased the same, once a backend serves it"
		if f.Status != nil && len(f.Status.Services) > 0 {
			n := len(f.Status.Services)
			svcs = fmt.Sprintf("while it leads: the VIPs of %d service%s on their pools' interfaces, leased the same, once a backend serves them", n, map[bool]string{true: "", false: "s"}[n == 1])
		}
		out = append(out,
			Finding{Area: "plan", Check: "service VIPs", State: Info, Detail: svcs},
			Finding{Area: "plan", Check: "IPVS", State: Info, Detail: "while it leads: the cluster's services and their backends, over netlink"},
			Finding{Area: "plan", Check: "neighbour entries", State: Info, Detail: "while it leads: each directly reached backend's address pinned to its MAC address"},
			Finding{Area: "plan", Check: "connection sync", State: Info, Detail: "IPVS's sync daemons on " + ifname + ": a backup while a member, a master too while it leads"},
			Finding{Area: "plan", Check: "/run/thelb/director", State: Info, Detail: "its socket and lock, root only"},
		)
		set("director")
	}
	if has(f, "backend") {
		out = append(out,
			Finding{Area: "plan", Check: "thelb0", State: Info, Detail: "a dummy interface, holding the VIPs of the services its offers are assigned to"},
			Finding{Area: "plan", Check: "nftables", State: Info, Detail: "the table inet thelb, only while a port mapping or a tunnelled service needs it"},
			Finding{Area: "plan", Check: "thelbt0", State: Info, Detail: "an IPIP interface, only while a tunnelled service is assigned; rp_filter 0 on it, and 2 for all if it was 1"},
			Finding{Area: "plan", Check: "/run/thelb/backend", State: Info, Detail: "its socket and lock, root only"},
		)
		set("backend")
	}
	return out
}
