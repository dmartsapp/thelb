//go:build linux

// Package backnet is a backend's kernel state, on Linux: theLB's own dummy
// interface, thelb0, without ARP, holding the VIPs of the services the backend
// is assigned to - so its kernel accepts the packets a director forwards -
// arp_ignore and arp_announce, so it never answers ARP for them or uses them
// as a source, and, where a service's port is not its application's, theLB's
// nftables table mapping one to the other. Restore removes thelb0, and with it every VIP, and puts the
// parameters back.
package backnet

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sync"
	"syscall"

	"github.com/dmartsapp/thelb/internal/ctlnet"
	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/netlink"
	"github.com/dmartsapp/thelb/internal/nft"
	"github.com/dmartsapp/thelb/internal/sysctl"
)

// Interface is theLB's dummy interface; TunnelInterface its IP-in-IP one.
const (
	Interface       = "thelb0"
	TunnelInterface = "thelbt0"
)

// Host is a backend's kernel state.
type Host struct {
	log    *logx.Logger
	mu     sync.Mutex
	index  int
	held   map[netip.Addr]bool
	maps   []nft.Map // what theLB's nftables table holds
	clamps []nft.Clamp
	tunnel bool
	params sysctl.Changes
}

// New is a backend's kernel state, not yet prepared.
func New(log *logx.Logger) *Host { return &Host{log: log, held: map[netip.Addr]bool{}} }

// Prepare creates thelb0 - removing one a crash left, and its VIPs - and sets
// the ARP parameters.
func (h *Host) Prepare() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ifc, err := net.InterfaceByName(Interface); err == nil {
		if err := netlink.DelLink(ifc.Index); err != nil {
			return fmt.Errorf("removing the %s an earlier run left: %w", Interface, err)
		}
		h.log.Warn("removed the "+Interface+" an earlier run left, with its VIPs", "interface", Interface)
	}
	if ifc, err := net.InterfaceByName(TunnelInterface); err == nil {
		if err := netlink.DelLink(ifc.Index); err != nil {
			return fmt.Errorf("removing the %s an earlier run left: %w", TunnelInterface, err)
		}
		h.log.Warn("removed the "+TunnelInterface+" an earlier run left", "interface", TunnelInterface)
	}
	if ok, err := nft.Exists(); err == nil && ok {
		if err := nft.Remove(); err != nil {
			return fmt.Errorf("removing the nftables table inet %s an earlier run left: %w", nft.Table, err)
		}
		h.log.Warn("removed the nftables table an earlier run left", "table", "inet "+nft.Table)
	}
	if err := netlink.AddDummy(Interface); err != nil {
		return fmt.Errorf("creating %s: %w (is the dummy module available?)", Interface, err)
	}
	ifc, err := net.InterfaceByName(Interface)
	if err != nil {
		return err
	}
	h.index = ifc.Index
	h.log.OK("interface created", "interface", Interface, "arp", "off")
	for _, x := range []struct {
		name, want string
		fine       []string
	}{
		{"net/ipv4/conf/all/arp_ignore", "1", []string{"1", "2"}},
		{"net/ipv4/conf/all/arp_announce", "2", nil},
	} {
		old, changed, err := h.params.Ensure(x.name, x.want, x.fine...)
		if err != nil {
			return err
		}
		if changed {
			h.log.OK("kernel parameter set", "name", x.name, "value", x.want, "was", old)
		}
	}
	return nil
}

// Hold makes thelb0 hold exactly vips.
func (h *Host) Hold(vips []netip.Addr) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	want := map[netip.Addr]bool{}
	for _, a := range vips {
		want[a.Unmap()] = true
	}
	var errs []error
	for a := range h.held {
		if !want[a] {
			if err := netlink.DelAddr(h.index, netip.PrefixFrom(a, a.BitLen())); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
				errs = append(errs, fmt.Errorf("removing %s from %s: %w", a, Interface, err))
				continue
			}
			delete(h.held, a)
			h.log.OK("VIP released", "vip", a, "interface", Interface)
		}
	}
	for a := range want {
		if h.held[a] {
			continue
		}
		var flags uint32
		if a.Is6() {
			flags = netlink.IFANoDAD
		}
		if err := netlink.AddAddr(h.index, netip.PrefixFrom(a, a.BitLen()), "", ctlnet.Proto, flags); err != nil && !errors.Is(err, syscall.EEXIST) {
			errs = append(errs, fmt.Errorf("adding %s to %s: %w", a, Interface, err))
			continue
		}
		h.held[a] = true
		h.log.OK("VIP held", "vip", a, "interface", Interface)
	}
	return errors.Join(errs...)
}

// Map makes theLB's nftables table map exactly maps - replacing it whole - and
// removes it when there are none: with matching ports there is no rule at all.
func (h *Host) Map(maps []nft.Map, clamps []nft.Clamp) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if slices.Equal(maps, h.maps) && slices.Equal(clamps, h.clamps) {
		return nil
	}
	if len(maps) == 0 && len(clamps) == 0 {
		if err := nft.Remove(); err != nil {
			return fmt.Errorf("removing the nftables table inet %s: %w", nft.Table, err)
		}
		h.log.OK("nothing to map or clamp: nftables table removed", "table", "inet "+nft.Table)
		h.maps, h.clamps = nil, nil
		return nil
	}
	if err := nft.Apply(maps, clamps...); err != nil {
		return fmt.Errorf("mapping ports in the nftables table inet %s: %w", nft.Table, err)
	}
	for _, m := range maps {
		if !slices.Contains(h.maps, m) {
			h.log.OK("port mapped", "vip", m.VIP, "proto", m.Proto, "port", m.Port, "to", m.AppPort, "table", "inet "+nft.Table)
		}
	}
	for _, c := range clamps {
		if !slices.Contains(h.clamps, c) {
			h.log.OK("TCP MSS clamped", "vip", c.VIP, "mss", c.MSS, "table", "inet "+nft.Table)
		}
	}
	h.maps, h.clamps = append([]nft.Map(nil), maps...), append([]nft.Clamp(nil), clamps...)
	return nil
}

// tunnelAddr is thelbt0's address: RFC 7600's IPv4 dummy address, for an
// interface that has no address of its own. Loose reverse-path filtering drops
// every packet that arrives on an interface without an IPv4 address - the
// kernel's no-address rule, whatever the route back - so thelbt0 needs one, or
// the packets it unwraps are dropped wherever filtering is on (Ubuntu's and
// RHEL's defaults). Nothing is sent from it: the replies go out the way the
// route to the client says.
var tunnelAddr = netip.MustParsePrefix("192.0.0.8/32")

// Tunnel creates or removes thelbt0: an IP-in-IP interface that takes packets
// from any director, and unwraps them for the VIPs on thelb0. Reverse-path
// filtering would drop them - their sources are clients, reached through
// another interface - so it is off on thelbt0, loose rather than strict for all
// interfaces if it was strict, and thelbt0 has an address (tunnelAddr).
func (h *Host) Tunnel(on bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if on == h.tunnel {
		return nil
	}
	if !on {
		if ifc, err := net.InterfaceByName(TunnelInterface); err == nil {
			if err := netlink.DelLink(ifc.Index); err != nil {
				return fmt.Errorf("removing %s: %w", TunnelInterface, err)
			}
		}
		h.tunnel = false
		h.log.OK("interface removed", "interface", TunnelInterface)
		return nil
	}
	if err := netlink.AddIPIPExternal(TunnelInterface); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("creating %s: %w (is the ipip module available?)", TunnelInterface, err)
	}
	// thelbt0's own parameter goes with it; the one for all interfaces is
	// recorded, and put back.
	if err := sysctl.Write("net/ipv4/conf/"+TunnelInterface+"/rp_filter", "0"); err != nil {
		return err
	}
	if old, changed, err := h.params.Ensure("net/ipv4/conf/all/rp_filter", "2", "0", "2"); err != nil {
		return err
	} else if changed {
		h.log.OK("kernel parameter set", "name", "net/ipv4/conf/all/rp_filter", "value", "2", "was", old)
	}
	if err := addTunnelAddr(); err != nil {
		return err
	}
	h.tunnel = true
	h.log.OK("interface created", "interface", TunnelInterface, "kind", "ipip, from any director")
	return nil
}

// addTunnelAddr gives thelbt0 its address, if it lacks it.
func addTunnelAddr() error {
	ifc, err := net.InterfaceByName(TunnelInterface)
	if err != nil {
		return err
	}
	if err := netlink.AddAddr(ifc.Index, tunnelAddr, TunnelInterface+":thelb", ctlnet.Proto, 0); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("adding %s to %s: %w", tunnelAddr.Addr(), TunnelInterface, err)
	}
	return nil
}

// hasTunnelAddr is whether thelbt0 has its address.
func hasTunnelAddr() bool {
	ifc, err := net.InterfaceByName(TunnelInterface)
	if err != nil {
		return false
	}
	addrs, err := netlink.Addrs()
	if err != nil {
		return true // cannot tell: nothing to repair
	}
	for _, a := range addrs {
		if a.Index == ifc.Index && a.Prefix == tunnelAddr {
			return true
		}
	}
	return false
}

// Repair puts back what someone removed or changed of the backend's kernel
// state - thelb0 and its VIPs, theLB's nftables table, thelbt0, the kernel
// parameters - and says what it put back.
func (h *Host) Repair() ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var fixed []string
	var errs []error
	ifc, err := net.InterfaceByName(Interface)
	if err != nil {
		if err := netlink.AddDummy(Interface); err != nil {
			return nil, fmt.Errorf("creating %s again: %w", Interface, err)
		}
		if ifc, err = net.InterfaceByName(Interface); err != nil {
			return nil, err
		}
		fixed = append(fixed, Interface+" was removed: created again")
	}
	h.index = ifc.Index
	on := map[netip.Addr]bool{}
	addrs, err := netlink.Addrs()
	if err != nil {
		return fixed, err
	}
	for _, a := range addrs {
		if a.Index == h.index {
			on[a.Prefix.Addr()] = true
		}
	}
	for a := range h.held {
		if on[a] {
			continue
		}
		var flags uint32
		if a.Is6() {
			flags = netlink.IFANoDAD
		}
		if err := netlink.AddAddr(h.index, netip.PrefixFrom(a, a.BitLen()), "", ctlnet.Proto, flags); err != nil && !errors.Is(err, syscall.EEXIST) {
			errs = append(errs, fmt.Errorf("adding %s to %s again: %w", a, Interface, err))
			continue
		}
		fixed = append(fixed, fmt.Sprintf("the VIP %s was removed from %s: added again", a, Interface))
	}
	if want := len(h.maps) + len(h.clamps); want > 0 {
		if n, err := nft.Rules(); err != nil {
			errs = append(errs, err)
		} else if n != want {
			if err := nft.Apply(h.maps, h.clamps...); err != nil {
				errs = append(errs, fmt.Errorf("the nftables table inet %s again: %w", nft.Table, err))
			} else {
				fixed = append(fixed, fmt.Sprintf("the nftables table inet %s held %d rules, not %d: replaced", nft.Table, n, want))
			}
		}
	}
	if h.tunnel {
		rp := "net/ipv4/conf/" + TunnelInterface + "/rp_filter"
		if _, err := net.InterfaceByName(TunnelInterface); err != nil {
			if err := netlink.AddIPIPExternal(TunnelInterface); err != nil && !errors.Is(err, syscall.EEXIST) {
				errs = append(errs, fmt.Errorf("creating %s again: %w", TunnelInterface, err))
			} else if err := sysctl.Write(rp, "0"); err != nil {
				errs = append(errs, err)
			} else if err := addTunnelAddr(); err != nil {
				errs = append(errs, err)
			} else {
				fixed = append(fixed, TunnelInterface+" was removed: created again")
			}
		} else if v, err := sysctl.Read(rp); err == nil && v != "0" {
			if err := sysctl.Write(rp, "0"); err != nil {
				errs = append(errs, err)
			} else {
				fixed = append(fixed, rp+" was "+v+": set to 0 again")
			}
		}
		if _, err := net.InterfaceByName(TunnelInterface); err == nil && !hasTunnelAddr() {
			if err := addTunnelAddr(); err != nil {
				errs = append(errs, err)
			} else {
				fixed = append(fixed, TunnelInterface+"'s address "+tunnelAddr.Addr().String()+" was removed: added again")
			}
		}
	}
	type param struct {
		name, want string
		fine       []string
	}
	ps := []param{
		{"net/ipv4/conf/all/arp_ignore", "1", []string{"1", "2"}},
		{"net/ipv4/conf/all/arp_announce", "2", nil},
	}
	if h.tunnel {
		ps = append(ps, param{"net/ipv4/conf/all/rp_filter", "2", []string{"0", "2"}})
	}
	for _, x := range ps {
		old, changed, err := h.params.Ensure(x.name, x.want, x.fine...)
		if err != nil {
			errs = append(errs, err)
		} else if changed {
			fixed = append(fixed, x.name+" was "+old+": set to "+x.want+" again")
		}
	}
	return fixed, errors.Join(errs...)
}

// Holding are the VIPs on thelb0.
func (h *Host) Holding() []netip.Addr {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []netip.Addr
	for a := range h.held {
		out = append(out, a)
	}
	return out
}

// Restore removes thelb0 - and with it the VIPs - thelbt0, and theLB's
// nftables table, and puts back the parameters theLB changed.
func (h *Host) Restore() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tunnel {
		if ifc, err := net.InterfaceByName(TunnelInterface); err == nil {
			if err := netlink.DelLink(ifc.Index); err != nil {
				h.log.Error("removing "+TunnelInterface+" failed", "error", err)
			} else {
				h.log.OK("interface removed", "interface", TunnelInterface)
			}
		}
		h.tunnel = false
	}
	if len(h.maps) > 0 || len(h.clamps) > 0 {
		if err := nft.Remove(); err != nil {
			h.log.Error("removing the nftables table failed", "table", "inet "+nft.Table, "error", err)
		} else {
			h.log.OK("nftables table removed", "table", "inet "+nft.Table)
		}
		h.maps, h.clamps = nil, nil
	}
	if h.index != 0 {
		if err := netlink.DelLink(h.index); err != nil {
			h.log.Error("removing "+Interface+" failed", "error", err)
		} else {
			h.log.OK("interface removed", "interface", Interface)
		}
		h.index, h.held = 0, map[netip.Addr]bool{}
	}
	for _, r := range h.params.Restore() {
		if r.Err != nil {
			h.log.Error("restoring a kernel parameter failed", "name", r.Name, "value", r.Value, "error", r.Err)
		} else {
			h.log.OK("kernel parameter restored", "name", r.Name, "value", r.Value)
		}
	}
}
