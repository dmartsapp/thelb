package join

import (
	"net"
	"net/netip"
	"testing"

	"github.com/dmartsapp/thelb/internal/cluster"
)

// FuzzProblem: a claim to join comes from anyone who reaches the control VIP
// with a secret. Whatever Problem lets through has a node's name, the role
// joined with, a fingerprint, a MAC address, a hostname and a version as a
// node reports them, and the connection's own address.
func FuzzProblem(f *testing.F) {
	f.Add("web-1", "backend", "backend", "0123456789abcdef", "52:54:00:00:00:01", "web-1.example", "0.1.0-alpha.3", "10.0.0.21", "10.0.0.21")
	f.Add("WEB", "director", "backend", "xyz", "52-54-00-00-00-01", "-bad", "", "::ffff:10.0.0.1", "10.0.0.1")
	f.Add("a\x00b", "backend", "backend", "0123456789abcdeF", "", "h\nx", "1\x1b[2J", "fe80::1%eth0", "fe80::1")
	f.Fuzz(func(t *testing.T, name, role, claimed, fp, mac, host, version, address, from string) {
		src, err := netip.ParseAddr(from)
		if err != nil {
			src = netip.Addr{}
		}
		c := cluster.Claim{Name: name, Role: claimed, Fingerprint: fp, MAC: mac, Hostname: host, Version: version, Address: address}
		if Problem(c, role, src) != "" {
			return
		}
		if !NameOK(name) || claimed != role || len(fp) != 16 || !hex(fp) || !HostnameOK(host) || !VersionOK(version) {
			t.Fatalf("let through: %+v as %q", c, role)
		}
		if mac != "" {
			if hw, err := net.ParseMAC(mac); err != nil || hw.String() != mac {
				t.Fatalf("let through the MAC %q", mac)
			}
		}
		a, err := netip.ParseAddr(address)
		if err != nil || a.Unmap() != src.Unmap() {
			t.Fatalf("let through the address %q from %v", address, src)
		}
		for _, s := range []string{name, host, version, mac, fp} {
			for _, r := range s {
				if r < 0x21 || r > 0x7e {
					t.Fatalf("let through %q", s)
				}
			}
		}
	})
}

// FuzzMemberOK: a member identity is the node's name, a hyphen and 8 to 32
// hexadecimal digits - nothing else reaches Raft's configuration.
func FuzzMemberOK(f *testing.F) {
	f.Add("lb-1-0123abcd", "lb-1")
	f.Add("lb-1-", "lb-1")
	f.Add("lb-2-0123abcd", "lb-1")
	f.Fuzz(func(t *testing.T, id, name string) {
		if !MemberOK(id, name) {
			return
		}
		suffix := id[len(name)+1:]
		if id[:len(name)] != name || id[len(name)] != '-' || len(suffix) < 8 || len(suffix) > 32 || !hex(suffix) {
			t.Fatalf("let through %q for %q", id, name)
		}
	})
}
