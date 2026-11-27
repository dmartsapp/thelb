package backend

import (
	"net/netip"
	"testing"

	"github.com/dmartsapp/thelb/internal/api"
)

// FuzzOrderOK: an assignment the leader sends; a backend plumbs its VIP on
// thelb0 and maps its ports. What orderOK takes is a host's address, a
// protocol theLB serves, and ports in range.
func FuzzOrderOK(f *testing.F) {
	f.Add("10.0.0.50", "tcp", 80, 8080)
	f.Add("0.0.0.0", "tcp", 80, 0)
	f.Add("224.0.0.1", "udp", 53, 53)
	f.Add("fe80::1%eth0", "sctp", 1, 65535)
	f.Add("::ffff:10.0.0.50", "icmp", 0, -1)
	f.Fuzz(func(t *testing.T, vip, proto string, port, app int) {
		v, ok := orderOK(api.Assignment{VIP: vip, Proto: proto, Port: port, AppPort: app})
		if !ok {
			return
		}
		if !v.IsValid() || v.IsUnspecified() || v.IsMulticast() || v.Zone() != "" || v.Is4In6() || v == netip.MustParseAddr("255.255.255.255") {
			t.Fatalf("took the VIP %q as %v", vip, v)
		}
		if (proto != "tcp" && proto != "udp" && proto != "sctp") || port < 1 || port > 65535 || app < 0 || app > 65535 {
			t.Fatalf("took %s %d -> %d", proto, port, app)
		}
	})
}
