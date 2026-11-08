//go:build linux

// Package dplane is the data plane of the director holding the service VIPs,
// on Linux: the VIPs on their interfaces, announced; IPVS services with their
// backends as direct-routing destinations; each backend's address pinned to
// its MAC address; and the IPVS parameters failover needs. It changes only
// what is theLB's, and Clear removes all of it.
package dplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/dmartsapp/thelb/internal/announce"
	"github.com/dmartsapp/thelb/internal/ctlnet"
	"github.com/dmartsapp/thelb/internal/director"
	"github.com/dmartsapp/thelb/internal/hostnet"
	"github.com/dmartsapp/thelb/internal/ipvs"
	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/netlink"
	"github.com/dmartsapp/thelb/internal/sysctl"
)

// The IPVS parameters a director holding services sets: a director taking over
// carries on with connections in progress, connections to a removed backend
// end at once, and a drained backend's persistence templates expire.
var params = []struct{ name, want string }{
	{"net/ipv4/vs/sloppy_tcp", "1"},
	{"net/ipv4/vs/expire_nodest_conn", "1"},
	{"net/ipv4/vs/expire_quiescent_template", "1"},
}

type heldVIP struct {
	index  int
	name   string
	cancel context.CancelFunc
}

type pin struct {
	index int
	mac   string
}

// Plane is the data plane.
type Plane struct {
	log *logx.Logger

	mu       sync.Mutex
	conn     *ipvs.Conn
	vips     map[netip.Addr]heldVIP
	services map[string]ipvs.Service // theLB's, by key
	pins     map[netip.Addr]pin
	params   sysctl.Changes
	tuned    bool
}

// New is an empty data plane.
func New(log *logx.Logger) *Plane {
	return &Plane{log: log, vips: map[netip.Addr]heldVIP{}, services: map[string]ipvs.Service{}, pins: map[netip.Addr]pin{}}
}

func label(ifname string, a netip.Addr) string {
	if l := ifname + ":thelb"; len(l) <= 15 && a.Is4() {
		return l
	}
	return ""
}

// Apply makes the kernel match the plan.
func (p *Plane) Apply(plan director.Plan) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var errs []error

	if len(plan.Services) > 0 {
		if p.conn == nil {
			c, err := ipvs.Open()
			if err != nil {
				return err
			}
			p.conn = c
		}
		if !p.tuned {
			for _, x := range params {
				old, changed, err := p.params.Ensure(x.name, x.want)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				if changed {
					p.log.OK("kernel parameter set", "name", x.name, "value", x.want, "was", old)
				}
			}
			p.tuned = len(errs) == 0
		}
	}

	// The VIPs.
	want := map[netip.Addr]string{}
	for _, v := range plan.VIPs {
		want[v.Addr] = v.Interface
	}
	for a, h := range p.vips {
		if ifname, ok := want[a]; !ok || ifname != h.name {
			p.dropVIP(a, h)
		}
	}
	for a, ifname := range want {
		if _, ok := p.vips[a]; ok {
			continue
		}
		ifc, err := net.InterfaceByName(ifname)
		if err != nil {
			errs = append(errs, fmt.Errorf("the VIP %s's interface %s: %w", a, ifname, err))
			continue
		}
		var flags uint32
		if a.Is6() {
			flags = netlink.IFANoDAD
		}
		if err := netlink.AddAddr(ifc.Index, netip.PrefixFrom(a, a.BitLen()), label(ifname, a), ctlnet.Proto, flags); err != nil && !errors.Is(err, syscall.EEXIST) {
			errs = append(errs, fmt.Errorf("adding the VIP %s to %s: %w", a, ifname, err))
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		p.vips[a] = heldVIP{index: ifc.Index, name: ifname, cancel: cancel}
		go p.announce(ctx, ifc, a)
		p.log.OK("service VIP taken", "vip", a, "interface", ifname)
	}

	// IPVS.
	if p.conn != nil {
		have := map[string]ipvs.Service{}
		if list, err := p.conn.Services(); err == nil {
			for _, s := range list {
				have[s.Key()] = s
			}
		} else {
			errs = append(errs, err)
		}
		desired := map[string]bool{}
		for _, ps := range plan.Services {
			key := ps.Key()
			desired[key] = true
			cur, exists := have[key]
			if !exists {
				if err := p.conn.AddService(ps.Service); err != nil {
					errs = append(errs, err)
					continue
				}
				p.log.OK("IPVS service added", "service", key, "scheduler", ps.Scheduler, "method", "direct")
			} else if cur.Scheduler != ps.Scheduler {
				if err := p.conn.UpdateService(ps.Service); err != nil {
					errs = append(errs, err)
				}
			}
			p.services[key] = ps.Service
			dests, err := p.conn.Dests(ps.Service)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			got := map[netip.AddrPort]ipvs.Dest{}
			for _, d := range dests {
				got[netip.AddrPortFrom(d.Addr, d.Port)] = d
			}
			wanted := map[netip.AddrPort]bool{}
			for _, d := range ps.Dests {
				ap := netip.AddrPortFrom(d.Addr, d.Port)
				wanted[ap] = true
				cur, ok := got[ap]
				switch {
				case !ok:
					if err := p.conn.AddDest(ps.Service, d); err != nil {
						errs = append(errs, err)
					} else {
						p.log.OK("IPVS destination added", "service", key, "backend", ap, "weight", d.Weight)
					}
				case cur.Weight != d.Weight:
					if err := p.conn.UpdateDest(ps.Service, d); err != nil {
						errs = append(errs, err)
					} else {
						p.log.OK("IPVS weight changed", "service", key, "backend", ap, "weight", d.Weight, "was", cur.Weight)
					}
				}
			}
			for ap, d := range got {
				if !wanted[ap] {
					if err := p.conn.DelDest(ps.Service, d); err != nil {
						errs = append(errs, err)
					} else {
						p.log.OK("IPVS destination removed", "service", key, "backend", ap)
					}
				}
			}
		}
		for key, s := range p.services {
			if !desired[key] {
				if err := p.conn.DelService(s); err != nil {
					errs = append(errs, err)
					continue
				}
				delete(p.services, key)
				p.log.OK("IPVS service removed", "service", key)
			}
		}
	}

	// The pinned backends.
	pins := map[netip.Addr]director.PlanPin{}
	for _, x := range plan.Pins {
		pins[x.Addr] = x
	}
	for a, old := range p.pins {
		if x, ok := pins[a]; !ok || x.MAC != old.mac {
			_ = netlink.UnpinNeighbour(old.index, a)
			delete(p.pins, a)
		}
	}
	for a, x := range pins {
		if _, ok := p.pins[a]; ok {
			continue
		}
		ifc, err := net.InterfaceByName(x.Interface)
		mac, merr := net.ParseMAC(x.MAC)
		if err != nil || merr != nil {
			errs = append(errs, fmt.Errorf("pinning %s: %v %v", a, err, merr))
			continue
		}
		if err := netlink.PinNeighbour(ifc.Index, a, mac); err != nil {
			errs = append(errs, fmt.Errorf("pinning %s to %s: %w", a, x.MAC, err))
			continue
		}
		p.pins[a] = pin{index: ifc.Index, mac: x.MAC}
		p.log.OK("backend pinned", "address", a, "mac", x.MAC, "interface", x.Interface)
	}
	return errors.Join(errs...)
}

func (p *Plane) announce(ctx context.Context, ifc *net.Interface, a netip.Addr) {
	start := time.Now()
	for _, at := range []time.Duration{0, time.Second, 2 * time.Second} {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(start.Add(at))):
		}
		if err := announce.Announce(ifc.Index, ifc.HardwareAddr, a); err != nil {
			p.log.Warn("announcing a service VIP failed", "vip", a, "interface", ifc.Name, "error", err)
			return
		}
	}
}

func (p *Plane) dropVIP(a netip.Addr, h heldVIP) {
	h.cancel()
	if err := netlink.DelAddr(h.index, netip.PrefixFrom(a, a.BitLen())); err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
		p.log.Error("removing a service VIP failed", "vip", a, "interface", h.name, "error", err)
	} else {
		p.log.OK("service VIP released", "vip", a, "interface", h.name)
	}
	delete(p.vips, a)
}

// Stats are IPVS's counters for theLB's services.
func (p *Plane) Stats() map[string]ipvs.Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]ipvs.Stats{}
	if p.conn == nil {
		return out
	}
	for _, s := range p.services {
		dests, err := p.conn.Dests(s)
		if err != nil {
			continue
		}
		for _, d := range dests {
			out[director.DestKey(s, netip.AddrPortFrom(d.Addr, d.Port))] = d.Stats
		}
	}
	return out
}

// Clear removes every service, VIP and pin theLB added, and restores the
// parameters it changed.
func (p *Plane) Clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, s := range p.services {
		if err := p.conn.DelService(s); err != nil {
			p.log.Error("removing an IPVS service failed", "service", key, "error", err)
		} else {
			p.log.OK("IPVS service removed", "service", key)
		}
		delete(p.services, key)
	}
	for a, h := range p.vips {
		p.dropVIP(a, h)
	}
	for a, x := range p.pins {
		_ = netlink.UnpinNeighbour(x.index, a)
		delete(p.pins, a)
	}
	for _, r := range p.params.Restore() {
		if r.Err != nil {
			p.log.Error("restoring a kernel parameter failed", "name", r.Name, "value", r.Value, "error", r.Err)
		} else {
			p.log.OK("kernel parameter restored", "name", r.Name, "value", r.Value)
		}
	}
	p.tuned = false
}

// Leftovers removes the plan's IPVS services and pinned backends that the
// kernel has and this data plane did not add.
func (p *Plane) Leftovers(plan director.Plan) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var errs []error
	if len(plan.Services) > 0 {
		c, err := ipvs.Open()
		if err == nil {
			have := map[string]bool{}
			if list, err := c.Services(); err == nil {
				for _, s := range list {
					have[s.Key()] = true
				}
			}
			for _, ps := range plan.Services {
				key := ps.Key()
				if _, ours := p.services[key]; ours || !have[key] {
					continue
				}
				if err := c.DelService(ps.Service); err != nil {
					errs = append(errs, err)
					continue
				}
				p.log.Warn("removed an IPVS service an earlier run left", "service", key)
			}
		}
	}
	for _, x := range plan.Pins {
		if _, ours := p.pins[x.Addr]; ours {
			continue
		}
		ifc, err := net.InterfaceByName(x.Interface)
		if err != nil {
			continue
		}
		e, ok, err := netlink.Neighbour(ifc.Index, x.Addr)
		if err != nil || !ok || e.State&netlink.NUDPermanent == 0 || e.MAC.String() != x.MAC {
			continue
		}
		if err := netlink.UnpinNeighbour(ifc.Index, x.Addr); err != nil {
			errs = append(errs, err)
			continue
		}
		p.log.Warn("removed a backend's pin an earlier run left", "address", x.Addr, "interface", x.Interface)
	}
	return errors.Join(errs...)
}

// Probe asks the network, on the interface that reaches addr, who has it.
func (p *Plane) Probe(ctx context.Context, addr netip.Addr) (string, error) {
	local, err := hostnet.For(addr, "")
	if err != nil {
		return "", err
	}
	ifc, err := net.InterfaceByName(local.Interface)
	if err != nil {
		return "", err
	}
	wait := time.Second
	if d, ok := ctx.Deadline(); ok && time.Until(d) < wait {
		wait = time.Until(d)
	}
	return announce.Probe(ifc.Index, ifc.HardwareAddr, local.Prefix.Addr(), addr, wait)
}
