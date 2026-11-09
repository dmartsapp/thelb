//go:build linux

package backnet

import (
	"bytes"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/netlink"
	"github.com/dmartsapp/thelb/internal/nft"
	"github.com/dmartsapp/thelb/internal/rootns"
	"github.com/dmartsapp/thelb/internal/sysctl"
)

// TestRootRepairPutsBackWhatWasRemoved: thelb0's VIPs, theLB's nftables table,
// thelbt0, a kernel parameter - and thelb0 itself.
func TestRootRepairPutsBackWhatWasRemoved(t *testing.T) {
	rootns.Enter(t)
	h := New(logx.New(&bytes.Buffer{}, "thelb"))
	if err := h.Prepare(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Restore)
	vip := netip.MustParseAddr("198.18.0.20")
	if err := h.Hold([]netip.Addr{vip}); err != nil {
		t.Fatal(err)
	}
	if err := h.Map([]nft.Map{{VIP: vip, Proto: "tcp", Port: 80, AppPort: 8080}}, []nft.Clamp{{VIP: vip, MSS: 1440}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Tunnel(true); err != nil {
		t.Fatal(err)
	}
	if fixed, err := h.Repair(); err != nil || len(fixed) != 0 {
		t.Fatalf("nothing drifted, and yet: %v %v", fixed, err)
	}
	if n, err := nft.Rules(); err != nil || n != 2 {
		t.Fatalf("the table's rules: %d %v", n, err)
	}
	held := func() bool {
		ifc, err := net.InterfaceByName(Interface)
		if err != nil {
			return false
		}
		addrs, _ := netlink.Addrs()
		for _, a := range addrs {
			if a.Index == ifc.Index && a.Prefix.Addr() == vip {
				return true
			}
		}
		return false
	}

	// Someone removes the VIP, the table and thelbt0, and changes arp_ignore.
	ifc, _ := net.InterfaceByName(Interface)
	if err := netlink.DelAddr(ifc.Index, netip.PrefixFrom(vip, 32)); err != nil {
		t.Fatal(err)
	}
	if err := nft.Remove(); err != nil {
		t.Fatal(err)
	}
	tun, _ := net.InterfaceByName(TunnelInterface)
	if err := netlink.DelLink(tun.Index); err != nil {
		t.Fatal(err)
	}
	if err := sysctl.Write("net/ipv4/conf/all/arp_ignore", "0"); err != nil {
		t.Fatal(err)
	}
	fixed, err := h.Repair()
	if err != nil || len(fixed) != 4 {
		t.Errorf("repaired %d: %q %v", len(fixed), fixed, err)
	}
	if n, _ := nft.Rules(); !held() || n != 2 {
		t.Errorf("after the repair: the VIP held %v, %d rules", held(), n)
	}
	if _, err := net.InterfaceByName(TunnelInterface); err != nil {
		t.Errorf("%s: %v", TunnelInterface, err)
	}
	if v, _ := sysctl.Read("net/ipv4/conf/all/arp_ignore"); v != "1" {
		t.Errorf("arp_ignore is %s", v)
	}

	// thelb0 itself: created again, with its VIP.
	ifc, _ = net.InterfaceByName(Interface)
	if err := netlink.DelLink(ifc.Index); err != nil {
		t.Fatal(err)
	}
	fixed, err = h.Repair()
	if err != nil || !held() || len(fixed) != 2 || !strings.Contains(fixed[0], "thelb0 was removed") {
		t.Errorf("thelb0 removed: %q %v, held %v", fixed, err, held())
	}
}
