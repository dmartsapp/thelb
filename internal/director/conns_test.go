package director

import (
	"net/netip"
	"testing"
)

// TestParseConns reads IPVS's connection table as the kernel writes it: IPv4
// addresses and every port in hexadecimal, IPv6 addresses written out.
func TestParseConns(t *testing.T) {
	table := `Pro FromIP   FPrt ToIP     TPrt DestIP   DPrt State       Expires PEName PEData
TCP C6120064 C9A2 C6120014 0050 C612001F 0050 ESTABLISHED   890
UDP C6120064 D431 C6120015 0035 C6120020 1F90 UDP           296
TCP fd74:6c62:0000:0000:0000:0000:0000:0064 C9A4 fd74:6c62:0000:0000:0000:0000:0000:0010 0050 fd74:6c62:0000:0000:0000:0000:0000:0031 0050 SYN_RECV       59
TCP nonsense
`
	got := ParseConns(table)
	if len(got) != 3 {
		t.Fatalf("parsed %d connections: %+v", len(got), got)
	}
	want := Conn{Proto: "TCP", Client: netip.MustParseAddrPort("198.18.0.100:51618"), VIP: netip.MustParseAddrPort("198.18.0.20:80"), Dest: netip.MustParseAddrPort("198.18.0.31:80"), State: "ESTABLISHED"}
	if got[0] != want {
		t.Errorf("IPv4: %+v, want %+v", got[0], want)
	}
	if got[1].Dest != netip.MustParseAddrPort("198.18.0.32:8080") || got[1].Proto != "UDP" {
		t.Errorf("UDP: %+v", got[1])
	}
	if got[2].Client != netip.MustParseAddrPort("[fd74:6c62::64]:51620") || got[2].Dest.Addr() != netip.MustParseAddr("fd74:6c62::31") || got[2].State != "SYN_RECV" {
		t.Errorf("IPv6: %+v", got[2])
	}
}
