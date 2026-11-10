//go:build linux

package ctlnet

import (
	"net/netip"
	"testing"
)

func TestGatewayFromTheKernelsRoutes(t *testing.T) {
	route := `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	0000A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
eth0	000013C6	010012C6	0003	0	0	0	00FFFFFF	0	0	0
ens3	00000000	0101A8C0	0003	0	0	100	00000000	0	0	0
ens3	0001A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
`
	for _, c := range []struct {
		ifname string
		want   string
		ok     bool
	}{
		{"ens3", "192.168.1.1", true}, // the default route's
		{"eth0", "198.18.0.1", true},  // no default route there: another route's gateway
		{"eth1", "", false},
	} {
		got, ok := gateway4(route, c.ifname)
		if ok != c.ok || (ok && got != netip.MustParseAddr(c.want)) {
			t.Errorf("gateway4(%s) = %v %v, want %s %v", c.ifname, got, ok, c.want, c.ok)
		}
	}
	route6 := `fd746c62000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth0
00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003     eth0
`
	if got, ok := gateway6(route6, "eth0"); !ok || got != netip.MustParseAddr("fe80::1") {
		t.Errorf("gateway6 = %v %v, want fe80::1", got, ok)
	}
	if _, ok := gateway6(route6, "eth1"); ok {
		t.Error("gateway6 found a gateway on an interface with no route")
	}
}
