package hostnet

import (
	"net/netip"
	"strings"
	"testing"
)

func machine(t *testing.T, addrs ...Address) {
	t.Helper()
	saved := List
	List = func() ([]Address, error) { return addrs, nil }
	t.Cleanup(func() { List = saved })
}

func TestForFindsTheInterfaceInTheVIPsNetwork(t *testing.T) {
	machine(t,
		Address{"lo", "", netip.MustParsePrefix("127.0.0.1/8")},
		Address{"eth0", "52:54:00:00:00:01", netip.MustParsePrefix("10.10.1.11/24")},
		Address{"eth1", "52:54:00:00:00:02", netip.MustParsePrefix("10.0.0.11/24")},
		Address{"eth1", "52:54:00:00:00:02", netip.MustParsePrefix("fd00::11/64")},
	)
	for vip, want := range map[string]string{"10.0.0.10": "eth1", "10.10.1.20": "eth0", "fd00::10": "eth1"} {
		a, err := For(netip.MustParseAddr(vip), "")
		if err != nil {
			t.Errorf("%s: %v", vip, err)
			continue
		}
		if a.Interface != want {
			t.Errorf("%s: on %s, want %s", vip, a.Interface, want)
		}
	}
}

func TestForSkipsTheVIPItselfAndHostRoutes(t *testing.T) {
	machine(t,
		Address{"eth1", "m", netip.MustParsePrefix("10.0.0.10/32")}, // the VIP, left by an earlier run
		Address{"eth1", "m", netip.MustParsePrefix("10.0.0.11/24")},
	)
	a, err := For(netip.MustParseAddr("10.0.0.10"), "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Prefix.Addr() != netip.MustParseAddr("10.0.0.11") {
		t.Errorf("picked %s, want the node's own 10.0.0.11", a.Prefix)
	}
}

func TestForNamedInterface(t *testing.T) {
	machine(t,
		Address{"eth0", "a", netip.MustParsePrefix("10.10.1.11/24")},
		Address{"eth1", "b", netip.MustParsePrefix("10.0.0.11/24")},
	)
	if _, err := For(netip.MustParseAddr("10.0.0.10"), "eth0"); err == nil || !strings.Contains(err.Error(), "no address in the network") {
		t.Errorf("eth0 for 10.0.0.10: %v", err)
	}
	if _, err := For(netip.MustParseAddr("10.0.0.10"), "eth9"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("eth9: %v", err)
	}
	if _, err := For(netip.MustParseAddr("192.168.9.9"), ""); err == nil || !strings.Contains(err.Error(), "--interface") {
		t.Errorf("no network: %v", err)
	}
	if a, err := For(netip.MustParseAddr("10.0.0.10"), "eth1"); err != nil || a.Interface != "eth1" {
		t.Errorf("eth1: %v %v", a, err)
	}
}

func TestListReadsThisMachine(t *testing.T) {
	addrs, err := List()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if !a.Prefix.IsValid() || a.Interface == "" {
			t.Errorf("a bad address: %+v", a)
		}
	}
}
