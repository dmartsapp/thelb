//go:build linux

package netlink

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/rootns"
)

func TestRootDummyAddressesAndNeighbours(t *testing.T) {
	rootns.Enter(t)
	if err := AddDummy("thelb0"); err != nil {
		t.Fatal(err)
	}
	ifc, err := net.InterfaceByName("thelb0")
	if err != nil {
		t.Fatal(err)
	}
	if ifc.Flags&net.FlagUp == 0 {
		t.Error("thelb0 is not up")
	}
	vip := netip.MustParsePrefix("198.18.0.20/32")
	if err := AddAddr(ifc.Index, vip, "thelb0", 116, 0); err != nil {
		t.Fatal(err)
	}
	addrs, err := Addrs()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range addrs {
		if a.Index == ifc.Index && a.Prefix == vip {
			found = true
			if a.HasProto && a.Proto != 116 {
				t.Errorf("proto %d", a.Proto)
			}
		}
	}
	if !found {
		t.Errorf("the VIP is not on thelb0: %+v", addrs)
	}

	// A neighbour entry, pinned and unpinned, on a dummy that has ARP.
	if err := AddDummy("thelbn"); err != nil {
		t.Fatal(err)
	}
	n, _ := net.InterfaceByName("thelbn")
	ip := netip.MustParseAddr("198.18.1.21")
	if err := AddAddr(n.Index, netip.MustParsePrefix("198.18.1.1/24"), "", 0, 0); err != nil {
		t.Fatal(err)
	}
	mac, _ := net.ParseMAC("52:54:00:00:00:21")
	if err := PinNeighbour(n.Index, ip, mac); err != nil {
		t.Fatal(err)
	}
	e, ok, err := Neighbour(n.Index, ip)
	if err != nil || !ok || e.State != NUDPermanent || e.MAC.String() != mac.String() {
		t.Errorf("pinned entry %+v %v %v", e, ok, err)
	}
	if err := UnpinNeighbour(n.Index, ip); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Neighbour(n.Index, ip); ok {
		t.Error("the entry is still there")
	}

	if err := DelLink(ifc.Index); err != nil {
		t.Fatal(err)
	}
	if _, err := net.InterfaceByName("thelb0"); err == nil {
		t.Error("thelb0 is still there")
	}
}

func TestRootAnIPIPTunnelThatTakesAnySender(t *testing.T) {
	rootns.Enter(t)
	if err := AddIPIPExternal("thelbt0"); err != nil {
		t.Skipf("no ipip module here: %v", err)
	}
	ifc, err := net.InterfaceByName("thelbt0")
	if err != nil {
		t.Fatal(err)
	}
	if ifc.Flags&net.FlagUp == 0 {
		t.Error("thelbt0 is not up")
	}
	if err := AddIPIPExternal("thelbt0"); err == nil {
		t.Error("a second one with the same name")
	}
	if err := DelLink(ifc.Index); err != nil {
		t.Fatal(err)
	}
}

// TestRootALeasedAddressLapses: an address put with a lifetime has it, a put
// again renews it, and the kernel removes it by itself once it lapses - what
// a theLB killed leaves answering for no longer than its lease.
func TestRootALeasedAddressLapses(t *testing.T) {
	rootns.Enter(t)
	if err := AddDummy("thelbl"); err != nil {
		t.Fatal(err)
	}
	ifc, _ := net.InterfaceByName("thelbl")
	vip := netip.MustParsePrefix("198.18.0.30/32")
	lifetime := func() (time.Duration, bool) {
		addrs, err := Addrs()
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range addrs {
			if a.Index == ifc.Index && a.Prefix == vip {
				return a.Lifetime, true
			}
		}
		return 0, false
	}
	if err := PutAddr(ifc.Index, vip, "thelbl:thelb", 116, 0, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if l, ok := lifetime(); !ok || l <= 0 || l > 2*time.Second {
		t.Fatalf("a leased address: here %v, lifetime %s", ok, l)
	}
	time.Sleep(1500 * time.Millisecond)
	if err := PutAddr(ifc.Index, vip, "thelbl:thelb", 116, 0, 2*time.Second); err != nil {
		t.Fatalf("renewing: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, ok := lifetime(); !ok {
		t.Fatal("renewed, the address lapsed all the same")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := lifetime(); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the kernel did not remove the address when its lease lapsed")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestRootManyLeasesOnOneSocket: PutAddrs puts a hundred leased addresses - more
// than one window of requests - each with its lifetime, and says which failed.
func TestRootManyLeasesOnOneSocket(t *testing.T) {
	rootns.Enter(t)
	if err := AddDummy("thelbm"); err != nil {
		t.Fatal(err)
	}
	ifc, _ := net.InterfaceByName("thelbm")
	var leases []Lease
	for i := range 100 {
		leases = append(leases, Lease{Index: ifc.Index, Prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{198, 18, 20, byte(i)}), 32), Label: "thelbm:thelb", Proto: 116})
	}
	leases = append(leases, Lease{Index: 9999, Prefix: netip.MustParsePrefix("198.18.21.1/32")})
	failed, err := PutAddrs(leases, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[netip.MustParseAddr("198.18.21.1")] == nil {
		t.Errorf("failed: %v; want only the address on no interface", failed)
	}
	addrs, err := Addrs()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range addrs {
		if a.Index == ifc.Index && a.Lifetime > 0 && a.Lifetime <= 30*time.Second {
			n++
		}
	}
	if n != 100 {
		t.Errorf("%d leased addresses on thelbm; want 100", n)
	}
}
