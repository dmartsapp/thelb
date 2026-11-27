package check

import (
	"strings"
	"testing"
)

// FuzzParse: a backend chooses its checks; they reach the leader's requests,
// status, logs and generated scripts. Whatever Parse takes is tcp, none, or an
// HTTP path of printable ASCII without spaces.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"", "tcp", "none", "http:/", "http:/healthz?x=1", "http:", "http:healthz", "http:/a b", "http:/\x1b[2J", "icmp"} {
		for _, p := range []string{"tcp", "udp", "sctp", ""} {
			f.Add(s, p)
		}
	}
	f.Fuzz(func(t *testing.T, spec, proto string) {
		got, err := Parse(spec, proto)
		if err != nil {
			return
		}
		switch {
		case got == "none":
		case got == "tcp" && proto == "tcp":
		case strings.HasPrefix(got, "http:/") && proto == "tcp" && len(got) <= len("http:")+maxPath:
			for _, r := range got {
				if r <= ' ' || r > '~' {
					t.Fatalf("took %q", got)
				}
			}
		default:
			t.Fatalf("Parse(%q, %q) = %q", spec, proto, got)
		}
	})
}
