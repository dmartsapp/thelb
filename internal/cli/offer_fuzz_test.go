package cli

import (
	"testing"

	"github.com/dmartsapp/thelb/internal/check"
	"github.com/dmartsapp/thelb/internal/cluster"
)

// FuzzParseOffer: --offer and thelb offer add. An offer parseOffer takes has a
// name the cluster takes, a protocol theLB serves, a port in range and a check
// for that protocol.
func FuzzParseOffer(f *testing.F) {
	for _, s := range []string{"tcp/80", "web=tcp/8080,check=http:/healthz", "udp/53", "dns=udp/53,check=tcp", "=tcp/80", "a=b=tcp/80", "tcp/0", "tcp/65536", "tcp/80,check=", "web=tcp/80,check=http:/\x1b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		o, err := parseOffer(spec)
		if err != nil {
			return
		}
		if !cluster.OfferNameOK(o.Name) || o.Port < 1 || o.Port > 65535 || (o.Proto != "tcp" && o.Proto != "udp" && o.Proto != "sctp") {
			t.Fatalf("%q took %+v", spec, o)
		}
		if got, err := check.Parse(o.Check, o.Proto); err != nil || got != o.Check {
			t.Fatalf("%q took the check %q", spec, o.Check)
		}
	})
}
