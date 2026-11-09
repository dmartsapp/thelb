//go:build linux

package ipvs

import (
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/dmartsapp/thelb/internal/netlink"
	"github.com/dmartsapp/thelb/internal/rootns"
)

func TestRootServicesAndDests(t *testing.T) {
	rootns.Enter(t)
	c, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	// IPVS takes a destination only where the namespace routes to it: a link
	// in the backends' network.
	if err := netlink.AddDummy("lab0"); err != nil {
		t.Fatal(err)
	}
	ifc, _ := net.InterfaceByName("lab0")
	vips := []string{"198.18.0.20"}
	if err := netlink.AddAddr(ifc.Index, netip.MustParsePrefix("198.18.0.1/24"), "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/proc/sys/net/ipv6"); err == nil {
		if err := netlink.AddAddr(ifc.Index, netip.MustParsePrefix("fd74:6c62::1/64"), "", 0, netlink.IFANoDAD); err != nil {
			t.Fatal(err)
		}
		vips = append(vips, "fd74:6c62::20")
	} else {
		t.Log("this kernel has no IPv6: IPv4 only")
	}
	for _, vip := range vips {
		s := Service{Addr: netip.MustParseAddr(vip), Protocol: TCP, Port: 80, Scheduler: "wrr"}
		if err := c.AddService(s); err != nil {
			t.Fatal(err)
		}
		backends := []string{"198.18.0.21", "198.18.0.22"}
		if strings.Contains(vip, ":") {
			backends = []string{"fd74:6c62::21", "fd74:6c62::22"}
		}
		for i, b := range backends {
			if err := c.AddDest(s, Dest{Addr: netip.MustParseAddr(b), Port: 80, Weight: uint32(i + 1)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.UpdateDest(s, Dest{Addr: netip.MustParseAddr(backends[0]), Port: 80, Weight: 0}); err != nil {
			t.Fatal(err)
		}
		svcs, err := c.Services()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, got := range svcs {
			if got.Key() == s.Key() && got.Scheduler == "wrr" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not listed: %+v", s.Key(), svcs)
		}
		dests, err := c.Dests(s)
		if err != nil {
			t.Fatal(err)
		}
		if len(dests) != 2 {
			t.Fatalf("dests %+v", dests)
		}
		for _, d := range dests {
			want := uint32(2)
			if d.Addr.String() == backends[0] {
				want = 0
			}
			if d.Weight != want || d.Port != 80 {
				t.Errorf("dest %+v, want weight %d", d, want)
			}
		}
	}
	s := Service{Addr: netip.MustParseAddr("198.18.0.20"), Protocol: TCP, Port: 80}
	if err := c.DelDest(s, Dest{Addr: netip.MustParseAddr("198.18.0.21"), Port: 80}); err != nil {
		t.Fatal(err)
	}
	if err := c.DelService(s); err != nil {
		t.Fatal(err)
	}
	if err := c.DelService(s); err != nil {
		t.Errorf("removing a service that is gone: %v", err)
	}
	svcs, _ := c.Services()
	if len(svcs) != len(vips)-1 {
		t.Errorf("after removal: %+v", svcs)
	}
}

// TestRootSyncDaemons: a master and a backup, each once, listed with their
// interface and sync ID, and stopped - a second stop is not an error.
func TestRootSyncDaemons(t *testing.T) {
	rootns.Enter(t)
	c, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddDummy("sync0"); err != nil {
		t.Fatal(err)
	}
	ifc, _ := net.InterfaceByName("sync0")
	if err := netlink.AddAddr(ifc.Index, netip.MustParsePrefix("198.18.0.11/24"), "", 0, 0); err != nil {
		t.Fatal(err)
	}
	backup := Daemon{Interface: "sync0", SyncID: 42}
	master := Daemon{Master: true, Interface: "sync0", SyncID: 42}
	for _, d := range []Daemon{backup, master} {
		if err := c.AddDaemon(d); err != nil {
			t.Fatalf("starting the %s: %v", d.Role(), err)
		}
	}
	t.Cleanup(func() { _ = c.DelDaemon(true); _ = c.DelDaemon(false) })
	if err := c.AddDaemon(master); err == nil {
		t.Error("a second master started")
	}
	ds, err := c.Daemons()
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]Daemon{}
	for _, d := range ds {
		roles[d.Role()] = d
	}
	if roles["master"] != master || roles["backup"] != backup || len(ds) != 2 {
		t.Errorf("the daemons: %+v", ds)
	}
	if err := c.DelDaemon(true); err != nil {
		t.Fatal(err)
	}
	if err := c.DelDaemon(true); err != nil {
		t.Errorf("stopping a master that is not running: %v", err)
	}
	if ds, _ := c.Daemons(); len(ds) != 1 || ds[0] != backup {
		t.Errorf("after stopping the master: %+v", ds)
	}
}
