//go:build linux

package dplane

import (
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/director"
	"github.com/dmartsapp/thelb/internal/ipvs"
	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/netlink"
	"github.com/dmartsapp/thelb/internal/rootns"
	"github.com/dmartsapp/thelb/internal/sysctl"
)

// syncLog is a log the data plane's announcing goroutines write to while the
// test reads it.
type syncLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncLog) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(b)
}

func (s *syncLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestRootApplyChangesOnlyWhatChangedAndRepairsDrift: the same plan again
// changes nothing and reads nothing; a change is applied alone; and the whole
// comparison puts back what someone removed or changed - and says so.
func TestRootApplyChangesOnlyWhatChangedAndRepairsDrift(t *testing.T) {
	rootns.Enter(t)
	saved := fullEvery
	fullEvery = time.Hour // whole comparisons only when the test asks
	t.Cleanup(func() { fullEvery = saved })
	if err := netlink.AddDummy("lab0"); err != nil {
		t.Fatal(err)
	}
	ifc, _ := net.InterfaceByName("lab0")
	if err := netlink.AddAddr(ifc.Index, netip.MustParsePrefix("198.18.0.1/24"), "", 0, 0); err != nil {
		t.Fatal(err)
	}
	var out syncLog
	p := New(logx.New(&out, "thelb"))
	t.Cleanup(p.Clear)

	vip := netip.MustParseAddr("198.18.0.20")
	b1, b2 := netip.MustParseAddr("198.18.0.21"), netip.MustParseAddr("198.18.0.22")
	svc := ipvs.Service{Addr: vip, Protocol: ipvs.TCP, Port: 80, Scheduler: "wrr"}
	plan := func(w2 uint32, reversed bool) director.Plan {
		dests := []ipvs.Dest{{Addr: b1, Port: 80, Weight: 1}, {Addr: b2, Port: 80, Weight: w2}}
		if reversed {
			dests[0], dests[1] = dests[1], dests[0]
		}
		return director.Plan{
			VIPs:     []director.PlanVIP{{Addr: vip, Interface: "lab0"}},
			Services: []director.PlanService{{Service: svc, Dests: dests}},
			Pins:     []director.PlanPin{{Addr: b1, MAC: "52:54:00:00:00:21", Interface: "lab0"}},
		}
	}
	apply := func(pl director.Plan, whole bool) {
		t.Helper()
		if whole {
			p.mu.Lock()
			p.lastFull = time.Time{}
			p.mu.Unlock()
		}
		if err := p.Apply(pl); err != nil {
			t.Fatal(err)
		}
	}
	c, err := ipvs.Open()
	if err != nil {
		t.Fatal(err)
	}
	weights := func() map[netip.Addr]uint32 {
		t.Helper()
		ds, err := c.Dests(svc)
		if err != nil {
			return nil
		}
		m := map[netip.Addr]uint32{}
		for _, d := range ds {
			m[d.Addr] = d.Weight
		}
		return m
	}
	vipOn := func() bool {
		addrs, _ := netlink.Addrs()
		for _, a := range addrs {
			if a.Index == ifc.Index && a.Prefix.Addr() == vip {
				return true
			}
		}
		return false
	}
	pinned := func() bool {
		e, ok, err := netlink.Neighbour(ifc.Index, b1)
		return err == nil && ok && e.State&netlink.NUDPermanent != 0 && e.MAC.String() == "52:54:00:00:00:21"
	}

	apply(plan(2, false), false)
	if p.fulls != 1 || !vipOn() || !pinned() || weights()[b2] != 2 {
		t.Fatalf("the first apply: fulls %d, vip %v, pin %v, weights %v", p.fulls, vipOn(), pinned(), weights())
	}
	// The same plan, in another order: nothing done, nothing read.
	apply(plan(2, true), false)
	if p.fulls != 1 {
		t.Errorf("the same plan compared the kernel whole: fulls %d", p.fulls)
	}
	// One change: applied alone, without reading the kernel.
	apply(plan(3, false), false)
	if p.fulls != 1 || weights()[b2] != 3 {
		t.Errorf("a new weight: fulls %d, weights %v", p.fulls, weights())
	}

	// Someone removes the IPVS service, the VIP and the pin: not seen until
	// the whole comparison, which puts all of it back.
	if err := c.DelService(svc); err != nil {
		t.Fatal(err)
	}
	if err := netlink.DelAddr(ifc.Index, netip.PrefixFrom(vip, 32)); err != nil {
		t.Fatal(err)
	}
	if err := netlink.UnpinNeighbour(ifc.Index, b1); err != nil {
		t.Fatal(err)
	}
	apply(plan(3, false), false)
	if weights() != nil {
		t.Errorf("an apply of the same plan read the kernel: %v", weights())
	}
	apply(plan(3, false), true)
	if w := weights(); p.fulls != 2 || w[b1] != 1 || w[b2] != 3 || !vipOn() || !pinned() {
		t.Errorf("after the whole comparison: fulls %d, weights %v, vip %v, pin %v", p.fulls, w, vipOn(), pinned())
	}
	for _, want := range []string{
		"WARN drift repaired: an IPVS service was removed; added again service=tcp/198.18.0.20:80",
		"WARN drift repaired: a service VIP was removed from its interface; adding it again vip=198.18.0.20",
		"WARN drift repaired: a backend's pinned neighbour entry was removed or changed; pinning it again address=198.18.0.21",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, out.String())
		}
	}

	// Someone changes a weight and adds a destination of their own, and puts
	// a parameter back: all undone.
	if err := c.UpdateDest(svc, ipvs.Dest{Addr: b1, Port: 80, Weight: 7}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddDest(svc, ipvs.Dest{Addr: netip.MustParseAddr("198.18.0.23"), Port: 80, Weight: 1}); err != nil {
		t.Fatal(err)
	}
	if err := sysctl.Write("net/ipv4/vs/sloppy_tcp", "0"); err != nil {
		t.Fatal(err)
	}
	apply(plan(3, false), true)
	if w := weights(); len(w) != 2 || w[b1] != 1 {
		t.Errorf("after the second whole comparison: weights %v", w)
	}
	if v, _ := sysctl.Read("net/ipv4/vs/sloppy_tcp"); v != "1" {
		t.Errorf("sloppy_tcp is %s", v)
	}
	for _, want := range []string{
		"WARN drift repaired: an IPVS weight was changed; set again service=tcp/198.18.0.20:80 backend=198.18.0.21:80 weight=1 was=7",
		"WARN drift repaired: an IPVS destination theLB did not add was in its service; removed service=tcp/198.18.0.20:80 backend=198.18.0.23:80",
		"WARN drift repaired: a kernel parameter was changed; set again name=net/ipv4/vs/sloppy_tcp value=1 was=0",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, out.String())
		}
	}
}
