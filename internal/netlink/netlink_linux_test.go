//go:build linux

package netlink

import (
	"net"
	"net/netip"
	"testing"

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
