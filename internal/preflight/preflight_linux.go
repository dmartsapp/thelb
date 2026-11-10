//go:build linux

package preflight

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/dmartsapp/thelb/internal/ipvs"
)

// Capabilities theLB uses (linux/capability.h).
const (
	capNetAdmin = 12
	capNetRaw   = 13
)

// Director checks a director's node: its capabilities, the kernel parameters
// it sets, IPVS, and IPv6 if the control VIP is IPv6. It returns what it found
// - the kernel, and how theLB adapts to it - or everything missing.
func Director(vip netip.Addr) (string, error) {
	var miss Error
	miss = append(miss, common(vip)...)
	if _, err := ipvs.Open(); err != nil {
		miss = append(miss, Missing{"IPVS", "the kernel has no IPVS: load it (sudo modprobe ip_vs), or install the kernel's extra modules (linux-modules-extra, kernel-modules-extra), then start theLB again"})
	} else if err := writable("/proc/sys/net/ipv4/vs/sloppy_tcp"); err != nil {
		miss = append(miss, Missing{"IPVS's kernel parameters", err.Error()})
	}
	if len(miss) > 0 {
		return "", miss
	}
	return found(), nil
}

// Backend checks a backend's node: its capabilities, the kernel parameters it
// sets, and IPv6 if the control VIP is IPv6.
func Backend(vip netip.Addr) (string, error) {
	if miss := common(vip); len(miss) > 0 {
		return "", miss
	}
	return found(), nil
}

func common(vip netip.Addr) Error {
	var miss Error
	eff, err := effective()
	if err != nil {
		miss = append(miss, Missing{"this process's capabilities", "unreadable: " + err.Error()})
	}
	for _, c := range []struct {
		bit  uint
		name string
	}{{capNetAdmin, "CAP_NET_ADMIN"}, {capNetRaw, "CAP_NET_RAW"}} {
		if err == nil && eff&(1<<c.bit) == 0 {
			miss = append(miss, Missing{c.name, "not among this process's capabilities: run theLB as root, and under systemd with " + c.name + " in CapabilityBoundingSet= (thelb generate systemd prints a unit that has it)"})
		}
	}
	if err := writable("/proc/sys/net/ipv4/conf/all/arp_ignore"); err != nil {
		miss = append(miss, Missing{"the kernel's network parameters (/proc/sys/net)", err.Error()})
	}
	if vip.Is6() {
		if _, err := os.Stat("/proc/sys/net/ipv6"); err != nil {
			miss = append(miss, Missing{"IPv6, for the control VIP " + vip.String(), "this kernel has no IPv6 (booted with ipv6.disable=1?): enable it, or give an IPv4 control VIP"})
		}
	}
	return miss
}

// effective is this process's effective capabilities, from /proc.
func effective() (uint64, error) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "CapEff:"); ok {
			return strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		}
	}
	return 0, errors.New("no CapEff line in /proc/self/status")
}

// writable is whether a kernel parameter can be set: opened for writing, with
// nothing written.
func writable(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	switch {
	case err == nil:
		return f.Close()
	case errors.Is(err, syscall.EROFS):
		return fmt.Errorf("read-only here - ProtectKernelTunables= in a systemd unit, or a container that mounts /proc/sys read-only: theLB sets kernel parameters while it runs")
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%s does not exist on this kernel", path)
	}
	return fmt.Errorf("%s cannot be set: %w", path, err)
}

// found describes the kernel. theLB marks its addresses both by their
// protocol (IFA_PROTO, Linux 6.1 and later) and, for IPv4, by their label, so
// it needs to know no more of it.
func found() string {
	var u syscall.Utsname
	release := "unknown"
	if syscall.Uname(&u) == nil {
		var b []byte
		for _, c := range u.Release {
			if c == 0 {
				break
			}
			b = append(b, byte(c))
		}
		release = string(b)
	}
	return "kernel " + release
}
