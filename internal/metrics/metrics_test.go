package metrics

import (
	"strings"
	"testing"

	"github.com/dmartsapp/thelb/internal/status"
)

func cluster() status.Status {
	term := uint64(7)
	return status.Status{
		Cluster: &status.Cluster{Name: "prod", Leader: "lb-1", Term: &term, Lease: 6},
		Nodes: []status.Node{
			{Node: "lb-1", Role: "leader", State: "up"}, {Node: "lb-2", Role: "director", State: "up"},
			{Node: "lb-3", Role: "director", State: "down"}, {Node: "web-1", Role: "backend", State: "up"},
		},
		VIPs:     []status.VIP{{VIP: "10.0.0.10", Purpose: "control", Interface: "eth0", HeldBy: "lb-1"}, {VIP: "10.0.0.20", Purpose: "service", Interface: "eth0", HeldBy: "lb-1"}},
		Services: []status.Service{{Service: "web", VIP: "10.0.0.20", BackendsUp: 1, Backends: 2}},
		Backends: []status.Backend{
			{Service: "web", VIP: "10.0.0.20", Backend: "web-1", OwnCheck: "ok", DirectorCheck: "ok", Weight: 1, Connections: 42, Packets: 420, Bytes: 4200, ActiveConnections: 3, InactiveConns: 1},
			{Service: "web", VIP: "10.0.0.20", Backend: "web-2", OwnCheck: "fail", DirectorCheck: "ok", Weight: 0, Connections: 8, Packets: 80, Bytes: 800},
		},
	}
}

// TestTheLeaderSaysWhatTheClusterServes: every director says how it stands;
// the leader alone says what the cluster serves, so nothing is counted twice.
func TestTheLeaderSaysWhatTheClusterServes(t *testing.T) {
	leader := Director(cluster(), "lb-1", 2)
	for _, want := range []string{
		"# TYPE thelb_leader gauge\nthelb_leader 1\n",
		"thelb_member 1\n", "thelb_raft_term 7\n", "thelb_vip_lease_seconds 6\n", "thelb_takeovers_total 2\n",
		`thelb_nodes{role="director",state="up"} 2`, `thelb_nodes{role="director",state="down"} 1`, `thelb_nodes{role="backend",state="up"} 1`,
		`thelb_vip_held{vip="10.0.0.20",purpose="service",interface="eth0"} 1`,
		`thelb_service_backends_up{service="web",vip="10.0.0.20"} 1`,
		`thelb_backend_weight{service="web",backend="web-2"} 0`,
		`thelb_backend_check_up{service="web",backend="web-2",check="own"} 0`,
		"# TYPE thelb_backend_connections_total counter\n", `thelb_backend_connections_total{service="web",backend="web-1"} 42`,
		`thelb_backend_connections_active{service="web",backend="web-1"} 3`, `thelb_backend_connections_inactive{service="web",backend="web-1"} 1`,
		`thelb_service_connections_total{service="web",vip="10.0.0.20"} 50`, `thelb_service_bytes_total{service="web",vip="10.0.0.20"} 5000`,
		`thelb_service_connections_active{service="web",vip="10.0.0.20"} 3`,
	} {
		if !strings.Contains(leader, want) {
			t.Errorf("the leader's metrics lack %q:\n%s", want, leader)
		}
	}
	follower := Director(cluster(), "lb-2", 0)
	if !strings.Contains(follower, "thelb_leader 0\n") || strings.Contains(follower, "thelb_backend_weight") || strings.Contains(follower, "thelb_vip_held") {
		t.Errorf("a follower's metrics: it leads, or it counts the cluster's backends too:\n%s", follower)
	}
}

func TestABackendSaysItsOffers(t *testing.T) {
	out := Backend(status.Status{Mine: &status.Mine{Role: "backend", State: "a member of the cluster", Offers: []status.MineOffer{
		{Offer: "web", Port: "tcp/80", Own: "ok", Service: "web"}, {Offer: "alt", Port: "tcp/8080", Own: "fail", Service: "-"},
	}}})
	for _, want := range []string{"thelb_member 1\n", `thelb_offer_check_up{offer="alt",port="tcp/8080"} 0`, `thelb_offer_assigned{offer="web",service="web"} 1`} {
		if !strings.Contains(out, want) {
			t.Errorf("the backend's metrics lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `offer="alt",service`) {
		t.Errorf("an unassigned offer is counted as assigned:\n%s", out)
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	var s set
	s.add("x", "gauge", "x").put(1, "name", "a\"b\\c\nd")
	if got := s.text(); !strings.Contains(got, `x{name="a\"b\\c\nd"} 1`) {
		t.Errorf("escaped: %s", got)
	}
}
