package director

import "testing"

// FuzzParseConns: IPVS's connection table, as the kernel writes it. The reader
// keeps only lines whose every address and port it read.
func FuzzParseConns(f *testing.F) {
	f.Add("Pro FromIP   FPrt ToIP     TPrt DestIP   DPrt State       Expires PEName PEData\nTCP C6120064 C5A4 C612000A 0050 C612001F 0050 ESTABLISHED     895\n")
	f.Add("h\nTCP fd00::1 0050 fd00::a 0050 fd00::1f 0050 SYN_RECV 10\n")
	f.Add("h\nTCP zzzz 0050 C612000A 0050 C612001F 0050 X 1\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, table string) {
		for _, c := range ParseConns(table) {
			if !c.Client.Addr().IsValid() || !c.VIP.Addr().IsValid() || !c.Dest.Addr().IsValid() {
				t.Fatalf("kept %+v", c)
			}
		}
	})
}
