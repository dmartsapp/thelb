package join

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/dmartsapp/thelb/internal/cluster"
)

// TestProblemChecksEveryWordOfAClaim: a joining node's claim reaches Raft,
// status tables and logs - each field must be what it says it is.
func TestProblemChecksEveryWordOfAClaim(t *testing.T) {
	from := netip.MustParseAddr("10.0.0.5")
	good := cluster.Claim{Name: "web-1", Role: "backend", Address: "10.0.0.5", MAC: "52:54:00:12:34:56", Fingerprint: "0123456789abcdef", Hostname: "web-1.example.com", Version: "0.1.0-alpha.2"}
	if p := Problem(good, "backend", from); p != "" {
		t.Fatalf("a good claim: %s", p)
	}
	noMAC := good
	noMAC.MAC, noMAC.Hostname = "", ""
	if p := Problem(noMAC, "backend", from); p != "" {
		t.Errorf("no MAC, no hostname: %s", p)
	}
	for _, c := range []struct {
		what string
		edit func(*cluster.Claim)
	}{
		{"a name with a newline", func(c *cluster.Claim) { c.Name = "web\n1" }},
		{"a MAC that is text", func(c *cluster.Claim) { c.MAC = "\x1b[31mred" }},
		{"an uppercase MAC", func(c *cluster.Claim) { c.MAC = "52:54:00:12:34:5A" }},
		{"a MAC with dashes", func(c *cluster.Claim) { c.MAC = "52-54-00-12-34-56" }},
		{"a hostname with an escape", func(c *cluster.Claim) { c.Hostname = "web\x1b]0;owned\x07" }},
		{"a hostname with a space", func(c *cluster.Claim) { c.Hostname = "web 1" }},
		{"a long hostname", func(c *cluster.Claim) { c.Hostname = strings.Repeat("a.", 130) + "a" }},
		{"a version with a newline", func(c *cluster.Claim) { c.Version = "0.1.0\nOK" }},
		{"no version", func(c *cluster.Claim) { c.Version = "" }},
		{"a long version", func(c *cluster.Claim) { c.Version = strings.Repeat("1", 65) }},
		{"another address", func(c *cluster.Claim) { c.Address = "10.0.0.6" }},
		{"a fingerprint that is not hex", func(c *cluster.Claim) { c.Fingerprint = "0123456789abcdeg" }},
	} {
		claim := good
		c.edit(&claim)
		if Problem(claim, "backend", from) == "" {
			t.Errorf("%s: accepted", c.what)
		}
	}
}

func TestMemberOK(t *testing.T) {
	for _, ok := range []string{"web-1-0a1b2c3d", "lb-00112233445566778899aabbccddeeff"} {
		name := ok[:strings.LastIndex(ok, "-")]
		if !MemberOK(ok, name) {
			t.Errorf("MemberOK(%q, %q) = false", ok, name)
		}
	}
	for _, bad := range []string{"web-1-", "web-1-0a1b", "web-1-0a1b2c3d\n", "web-1-0A1B2C3D", "web-2-0a1b2c3d", "web-1-0a1b2c3d-x", "web-1-" + strings.Repeat("a", 33)} {
		if MemberOK(bad, "web-1") {
			t.Errorf("MemberOK(%q) = true", bad)
		}
	}
}
