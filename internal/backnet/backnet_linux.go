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

// Interface is theLB's dummy interface.
const Interface = "thelb0"

// Host is a backend's kernel state.
type Host struct {
	log    *logx.Logger
	mu     sync.Mutex
	index  int
	held   map[netip.Addr]bool
	maps   []nft.Map // what theLB's nftables table holds
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
func (h *Host) Map(maps []nft.Map) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if slices.Equal(maps, h.maps) {
		return nil
	}
	if len(maps) == 0 {
		if err := nft.Remove(); err != nil {
			return fmt.Errorf("removing the nftables table inet %s: %w", nft.Table, err)
		}
		h.log.OK("ports no longer mapped: nftables table removed", "table", "inet "+nft.Table)
		h.maps = nil
		return nil
	}
	if err := nft.Apply(maps); err != nil {
		return fmt.Errorf("mapping ports in the nftables table inet %s: %w", nft.Table, err)
	}
	for _, m := range maps {
		if !slices.Contains(h.maps, m) {
			h.log.OK("port mapped", "vip", m.VIP, "proto", m.Proto, "port", m.Port, "to", m.AppPort, "table", "inet "+nft.Table)
		}
	}
	h.maps = append([]nft.Map(nil), maps...)
	return nil
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

// Restore removes thelb0 - and with it the VIPs - and theLB's nftables table,
// and puts back the parameters theLB changed.
func (h *Host) Restore() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.maps) > 0 {
		if err := nft.Remove(); err != nil {
			h.log.Error("removing the nftables table failed", "table", "inet "+nft.Table, "error", err)
		} else {
			h.log.OK("nftables table removed", "table", "inet "+nft.Table)
		}
		h.maps = nil
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
