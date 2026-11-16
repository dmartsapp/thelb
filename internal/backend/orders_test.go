package backend

import (
	"testing"

	"github.com/dmartsapp/thelb/internal/api"
)

// TestOrdersAreCheckedBeforeTheKernel: a backend plumbs VIPs and writes
// nftables rules from its orders - only from orders that are ones.
func TestOrdersAreCheckedBeforeTheKernel(t *testing.T) {
	good := api.Assignment{Service: "web", VIP: "10.0.0.20", Proto: "tcp", Port: 80, AppPort: 8080}
	if _, ok := orderOK(good); !ok {
		t.Fatal("a good order: refused")
	}
	for what, edit := range map[string]func(*api.Assignment){
		"no VIP":           func(a *api.Assignment) { a.VIP = "" },
		"a VIP with zone":  func(a *api.Assignment) { a.VIP = "fe80::1%eth0" },
		"a broadcast VIP":  func(a *api.Assignment) { a.VIP = "255.255.255.255" },
		"a multicast VIP":  func(a *api.Assignment) { a.VIP = "224.0.0.81" },
		"an unspecified":   func(a *api.Assignment) { a.VIP = "0.0.0.0" },
		"another protocol": func(a *api.Assignment) { a.Proto = "icmp" },
		"port 0":           func(a *api.Assignment) { a.Port = 0 },
		"a port past":      func(a *api.Assignment) { a.Port = 65616 },
		"an app port past": func(a *api.Assignment) { a.AppPort = 70000 },
	} {
		a := good
		edit(&a)
		if _, ok := orderOK(a); ok {
			t.Errorf("an order with %s: taken", what)
		}
	}
}
