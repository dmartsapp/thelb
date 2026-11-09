//go:build linux

package nft

import (
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/dmartsapp/thelb/internal/rootns"
)

// list is what nft, run in this test's network namespace, says the table holds.
func list(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("nft")
	if err != nil {
		return ""
	}
	out, _ := exec.Command(path, "list", "table", "inet", Table).CombinedOutput()
	return string(out)
}

func TestRootTheTableIsReplacedWhole(t *testing.T) {
	rootns.Enter(t)
	maps := []Map{
		{VIP: netip.MustParseAddr("198.18.0.20"), Proto: "tcp", Port: 80, AppPort: 8080},
		{VIP: netip.MustParseAddr("198.18.0.21"), Proto: "udp", Port: 53, AppPort: 5353},
		{VIP: netip.MustParseAddr("198.18.0.21"), Proto: "sctp", Port: 2905, AppPort: 2906},
	}
	if _, err := os.Stat("/proc/sys/net/ipv6"); err == nil {
		maps = append(maps, Map{VIP: netip.MustParseAddr("fd74:6c62::20"), Proto: "tcp", Port: 443, AppPort: 8443})
	}
	if err := Apply(maps); err != nil {
		t.Fatal(err)
	}
	if ok, err := Exists(); err != nil || !ok {
		t.Fatalf("the table after Apply: %v %v", ok, err)
	}
	if out := list(t); out == "" {
		t.Log("no nft command here: the table's contents are not checked")
	} else {
		t.Logf("nft list table inet thelb:\n%s", out)
		for _, want := range []string{"chain portmap", "type nat hook prerouting priority dstnat", "198.18.0.20", "dport 80", ":8080", "dport 53", ":5353", "dport 2905", ":2906"} {
			if !strings.Contains(out, want) {
				t.Errorf("nft list lacks %q:\n%s", want, out)
			}
		}
	}
	// Clamping the MSS of handshakes from a tunnelled VIP.
	if err := Apply(maps[:1], Clamp{VIP: netip.MustParseAddr("198.18.0.22"), MSS: 1440}); err != nil {
		t.Fatal(err)
	}
	if out := list(t); out != "" {
		for _, want := range []string{"chain mss", "type filter hook output priority mangle", "ip saddr 198.18.0.22", "tcp option maxseg size > 1440", "tcp option maxseg size set 1440", ":8080"} {
			if !strings.Contains(out, want) {
				t.Errorf("nft list lacks %q:\n%s", want, out)
			}
		}
		t.Logf("with a clamp:\n%s", out)
	}
	// Applied again with fewer: the old rules are gone.
	if err := Apply(maps[:1]); err != nil {
		t.Fatal(err)
	}
	if out := list(t); out != "" && (strings.Contains(out, ":5353") || !strings.Contains(out, ":8080")) {
		t.Errorf("after a smaller Apply:\n%s", out)
	}
	if err := Remove(); err != nil {
		t.Fatal(err)
	}
	if ok, err := Exists(); err != nil || ok {
		t.Errorf("the table after Remove: %v %v", ok, err)
	}
	if err := Remove(); err != nil {
		t.Errorf("removing a table that is gone: %v", err)
	}
}
