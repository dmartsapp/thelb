//go:build linux

// Package dplane is the data plane of the director holding the service VIPs,
// on Linux: the VIPs on their interfaces, announced; IPVS services with their
// backends as direct-routing destinations; each backend's address pinned to
// its MAC address; and the IPVS parameters failover needs. It changes only
// what is theLB's, and Clear removes all of it.
//
// Apply changes only what differs from the plan it applied last - nothing at
// all when the plan is the same - and compares the whole kernel with the plan
// every 10 s, putting back what someone removed or changed: drift repaired,
// and logged.
package dplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"strings"
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

	syncs    map[bool]ipvs.Daemon // the sync daemons this data plane started, by master
	last     *director.Plan       // the plan applied last
	lastFull time.Time            // when the kernel was last compared whole
	dirty    bool                 // the last apply failed: compare whole next time
	fulls    int                  // whole comparisons made, for tests
}

// fullEvery is how often Apply compares the whole kernel with the plan.
var fullEvery = 10 * time.Second

// New is an empty data plane.
func New(log *logx.Logger) *Plane {
	return &Plane{log: log, vips: map[netip.Addr]heldVIP{}, services: map[string]ipvs.Service{}, pins: map[netip.Addr]pin{}, syncs: map[bool]ipvs.Daemon{}}
}

func label(ifname string, a netip.Addr) string {
	if l := ifname + ":thelb"; len(l) <= 15 && a.Is4() {
		return l
	}
	return ""
}

// Apply makes the kernel match the plan: only what differs from the last plan,
// or - every fullEvery, and after a failure - all of it, read from the kernel.
func (p *Plane) Apply(plan director.Plan) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	full := p.dirty || p.last == nil || time.Since(p.lastFull) >= fullEvery
	if !full && samePlan(*p.last, plan) {
		return nil
	}
	err := p.apply(plan, full)
	cp := clonePlan(plan)
	p.last = &cp
	if full {
		p.lastFull = time.Now()
		p.fulls++
	}
	p.dirty = err != nil
	return err
}

// samePlan is whether two plans hold the same, in whatever order.
func samePlan(a, b director.Plan) bool { return reflect.DeepEqual(canonical(a), canonical(b)) }

func canonical(p director.Plan) director.Plan {
	c := clonePlan(p)
	slices.SortFunc(c.VIPs, func(x, y director.PlanVIP) int { return x.Addr.Compare(y.Addr) })
	slices.SortFunc(c.Pins, func(x, y director.PlanPin) int { return x.Addr.Compare(y.Addr) })
	slices.SortFunc(c.Services, func(x, y director.PlanService) int { return strings.Compare(x.Key(), y.Key()) })
	for i := range c.Services {
		slices.SortFunc(c.Services[i].Dests, func(x, y ipvs.Dest) int {
			return netip.AddrPortFrom(x.Addr, x.Port).Compare(netip.AddrPortFrom(y.Addr, y.Port))
		})
	}
	return c
}

func clonePlan(p director.Plan) director.Plan {
	c := director.Plan{VIPs: slices.Clone(p.VIPs), Pins: slices.Clone(p.Pins), Services: make([]director.PlanService, len(p.Services))}
	for i, s := range p.Services {
		c.Services[i] = director.PlanService{Service: s.Service, Dests: slices.Clone(s.Dests)}
	}
	return c
}

func (p *Plane) apply(plan director.Plan, full bool) error {
	var errs []error
	// What was applied last: in a whole comparison, what the kernel lacks of
	// it was removed by someone else - drift, repaired and logged as such.
	lastServices := map[string]director.PlanService{}
	if p.last != nil {
		for _, ps := range p.last.Services {
			lastServices[ps.Key()] = ps
		}
	}
	lastDest := func(key string, ap netip.AddrPort) (ipvs.Dest, bool) {
		for _, d := range lastServices[key].Dests {
			if netip.AddrPortFrom(d.Addr, d.Port) == ap {
				return d, true
			}
		}
		return ipvs.Dest{}, false
	}

	if len(plan.Services) > 0 {
		if p.conn == nil {
			c, err := ipvs.Open()
			if err != nil {
				return err
			}
			p.conn = c
		}
		if !p.tuned || full {
			for _, x := range params {
				old, changed, err := p.params.Ensure(x.name, x.want)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				switch {
				case changed && p.tuned:
					p.log.Warn("drift repaired: a kernel parameter was changed; set again", "name", x.name, "value", x.want, "was", old)
				case changed:
					p.log.OK("kernel parameter set", "name", x.name, "value", x.want, "was", old)
				}
			}
			p.tuned = len(errs) == 0
		}
	}

	// The VIPs that go, first: nothing more arrives for what goes next.
	want := map[netip.Addr]string{}
	for _, v := range plan.VIPs {
		want[v.Addr] = v.Interface
	}
	for a, h := range p.vips {
		if ifname, ok := want[a]; !ok || ifname != h.name {
			p.dropVIP(a, h)
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
		if held, ok := p.pins[a]; ok {
			if !full {
				continue
			}
			if e, found, err := netlink.Neighbour(held.index, a); err != nil || (found && e.State&netlink.NUDPermanent != 0 && e.MAC.String() == held.mac) {
				continue
			}
			delete(p.pins, a)
			p.log.Warn("drift repaired: a backend's pinned neighbour entry was removed or changed; pinning it again", "address", a, "mac", x.MAC)
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
	// IPVS.
	if p.conn != nil {
		// What the kernel has: read from it in a whole comparison, taken from
		// the last plan applied otherwise.
		have := map[string]ipvs.Service{}
		if full {
			if list, err := p.conn.Services(); err == nil {
				for _, s := range list {
					have[s.Key()] = s
				}
			} else {
				errs = append(errs, err)
			}
		} else {
			for k, ps := range lastServices {
				have[k] = ps.Service
			}
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
				if _, had := lastServices[key]; had && full {
					p.log.Warn("drift repaired: an IPVS service was removed; added again", "service", key, "scheduler", ps.Scheduler)
				} else {
					p.log.OK("IPVS service added", "service", key, "scheduler", ps.Scheduler, "method", "direct")
				}
			} else if cur.Scheduler != ps.Scheduler {
				if err := p.conn.UpdateService(ps.Service); err != nil {
					errs = append(errs, err)
				}
			}
			p.services[key] = ps.Service
			var dests []ipvs.Dest
			switch {
			case !exists: // just added: none yet
			case full:
				var err error
				if dests, err = p.conn.Dests(ps.Service); err != nil {
					errs = append(errs, err)
					continue
				}
			default:
				dests = lastServices[key].Dests
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
					} else if _, had := lastDest(key, ap); had && full && exists {
						p.log.Warn("drift repaired: an IPVS destination was removed; added again", "service", key, "backend", ap, "weight", d.Weight)
					} else {
						p.log.OK("IPVS destination added", "service", key, "backend", ap, "weight", d.Weight, "method", method(d))
					}
				case cur.Tunnel != d.Tunnel:
					// The forwarding method is the destination's: remove and add it again.
					if err := p.conn.DelDest(ps.Service, cur); err != nil {
						errs = append(errs, err)
					} else if err := p.conn.AddDest(ps.Service, d); err != nil {
						errs = append(errs, err)
					} else {
						p.log.OK("IPVS destination's method changed", "service", key, "backend", ap, "tunnel", d.Tunnel)
					}
				case cur.Weight != d.Weight:
					if err := p.conn.UpdateDest(ps.Service, d); err != nil {
						errs = append(errs, err)
					} else if was, had := lastDest(key, ap); had && full && was.Weight == d.Weight {
						p.log.Warn("drift repaired: an IPVS weight was changed; set again", "service", key, "backend", ap, "weight", d.Weight, "was", cur.Weight)
					} else {
						p.log.OK("IPVS weight changed", "service", key, "backend", ap, "weight", d.Weight, "was", cur.Weight)
					}
				}
			}
			for ap, d := range got {
				if !wanted[ap] {
					if err := p.conn.DelDest(ps.Service, d); err != nil {
						errs = append(errs, err)
					} else if _, had := lastDest(key, ap); !had && full {
						p.log.Warn("drift repaired: an IPVS destination theLB did not add was in its service; removed", "service", key, "backend", ap)
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

	// The VIPs that come, last: a packet a VIP takes in finds IPVS ready for it.
	// In a whole comparison, a VIP held but no longer on its interface was
	// removed by someone: it is added, and announced, again.
	// By interface and address: a director's host that is also a backend has
	// the VIP on thelb0 too.
	type on struct {
		index int
		addr  netip.Addr
	}
	var present map[on]bool
	if full && len(p.vips) > 0 {
		if addrs, err := netlink.Addrs(); err == nil {
			present = map[on]bool{}
			for _, x := range addrs {
				present[on{x.Index, x.Prefix.Addr()}] = true
			}
		} else {
			errs = append(errs, err)
		}
	}
	for a, ifname := range want {
		if h, ok := p.vips[a]; ok {
			if present == nil || present[on{h.index, a}] {
				continue
			}
			h.cancel()
			delete(p.vips, a)
			p.log.Warn("drift repaired: a service VIP was removed from its interface; adding it again", "vip", a, "interface", ifname)
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

	return errors.Join(errs...)
}

func method(d ipvs.Dest) string {
	if d.Tunnel {
		return "tunnel"
	}
	return "direct"
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
	p.last, p.dirty = nil, false
	for _, r := range p.params.Restore() {
		if r.Err != nil {
			p.log.Error("restoring a kernel parameter failed", "name", r.Name, "value", r.Value, "error", r.Err)
		} else {
			p.log.OK("kernel parameter restored", "name", r.Name, "value", r.Value)
		}
	}
	p.tuned = false
}

// Sync makes IPVS's connection-synchronisation daemons what s says: each of the
// master and the backup started, with s's interface and sync ID, or stopped. A
// daemon this data plane did not start was left by an earlier run (a crash
// leaves the kernel threads behind, like the VIPs): it is replaced, or removed,
// and said so; one it started and finds gone was stopped by someone else, and is
// started again - drift, repaired.
func (p *Plane) Sync(s director.SyncState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil {
		if !s.Master && !s.Backup && len(p.syncs) == 0 {
			return nil // nothing started, nothing to stop: IPVS need not be there
		}
		c, err := ipvs.Open()
		if err != nil {
			return err
		}
		p.conn = c
	}
	running, err := p.conn.Daemons()
	if err != nil {
		return err
	}
	have := map[bool]ipvs.Daemon{}
	for _, d := range running {
		have[d.Master] = d
	}
	var errs []error
	for _, master := range []bool{true, false} {
		want := ipvs.Daemon{Master: master, Interface: s.Interface, SyncID: s.SyncID}
		wanted := (master && s.Master) || (!master && s.Backup)
		cur, on := have[master]
		mine, started := p.syncs[master]
		ours := started && on && cur == mine
		switch {
		case on && (!wanted || cur != want):
			if err := p.conn.DelDaemon(master); err != nil {
				errs = append(errs, err)
				continue
			}
			if ours {
				p.log.OK("IPVS connection sync stopped", "role", cur.Role())
			} else {
				p.log.Warn("removed an IPVS connection sync daemon an earlier run left", "role", cur.Role(), "interface", cur.Interface, "sync_id", cur.SyncID)
			}
			delete(p.syncs, master)
			on = false
		case !on && started && !wanted:
			delete(p.syncs, master)
		}
		if wanted && on && !started {
			p.syncs[master] = cur // left by an earlier run, as wanted: kept
			p.log.OK("IPVS connection sync running, as an earlier run left it", "role", cur.Role(), "interface", cur.Interface, "sync_id", cur.SyncID)
		}
		if wanted && !on {
			if err := p.conn.AddDaemon(want); err != nil {
				errs = append(errs, err)
				continue
			}
			if started && mine == want && !ours {
				p.log.Warn("drift repaired: the IPVS connection sync daemon was stopped; started again", "role", want.Role(), "interface", want.Interface, "sync_id", want.SyncID)
			} else {
				p.log.OK("IPVS connection sync started", "role", want.Role(), "interface", want.Interface, "sync_id", want.SyncID)
			}
			p.syncs[master] = want
		}
	}
	return errors.Join(errs...)
}

// Leftovers removes the plan's IPVS services and pinned backends that the
// kernel has and this data plane did not add.
func (p *Plane) Leftovers(plan director.Plan) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var errs []error
	if len(plan.Services) > 0 || len(plan.Pools) > 0 {
		c, err := ipvs.Open()
		if err == nil {
			// The cluster's services, wherever their VIPs are; and any service
			// on a pool's address - theLB's, though the cluster no longer has it.
			known := map[string]bool{}
			for _, ps := range plan.Services {
				known[ps.Key()] = true
			}
			inPool := func(a netip.Addr) bool {
				for i := range plan.Pools {
					if plan.Pools[i].Contains(a) {
						return true
					}
				}
				return false
			}
			list, _ := c.Services()
			for _, s := range list {
				key := s.Key()
				if _, ours := p.services[key]; ours || (!known[key] && !inPool(s.Addr)) {
					continue
				}
				if err := c.DelService(s); err != nil {
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
