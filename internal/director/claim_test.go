package director

import (
	"bytes"
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/logx"
)

// answering is a data plane whose network has some VIPs answered for.
type answering struct {
	nothing
	who   map[netip.Addr]string
	looks int
}

func (a *answering) ProbeAll(context.Context, []PlanVIP) (map[netip.Addr]string, error) {
	a.looks++
	return a.who, nil
}

// TestAServiceVIPIsTakenOnlyOnceItIsNoMore: a new leader takes a service VIP
// only after two looks, a second or more apart, found no machine answering for
// it; one another machine answers for is never taken, and the log says why; a
// VIP the kernel holds already stays, without a look.
func TestAServiceVIPIsTakenOnlyOnceItIsNoMore(t *testing.T) {
	var logs bytes.Buffer
	net := &answering{who: map[netip.Addr]string{netip.MustParseAddr("10.0.0.21"): "52:54:00:aa:bb:cc"}}
	s := &session{n: &node{plane: net}, log: logx.New(&logs, "thelb"), fsm: cluster.New()}
	p := &plane{s: s, held: map[netip.Addr]bool{}, freeSince: map[netip.Addr]time.Time{}, answered: map[netip.Addr]time.Time{}, said: map[netip.Addr]string{}}
	free, taken := PlanVIP{Addr: netip.MustParseAddr("10.0.0.20"), Interface: "eth0"}, PlanVIP{Addr: netip.MustParseAddr("10.0.0.21"), Interface: "eth0"}
	want := []PlanVIP{free, taken}

	if got := p.claim(context.Background(), want); len(got) != 0 {
		t.Fatalf("taken at the first look: %v", got)
	}
	if got := p.claim(context.Background(), want); len(got) != 0 {
		t.Fatalf("taken at a second look less than a second later: %v", got)
	}
	time.Sleep(1100 * time.Millisecond)
	got := p.claim(context.Background(), want)
	if len(got) != 1 || got[0] != free {
		t.Fatalf("after a second, claimed %v; want %v only", got, free)
	}
	if !strings.Contains(logs.String(), "WARN another machine answers for a service VIP: this leader waits vip=10.0.0.21") {
		t.Errorf("the log does not say why 10.0.0.21 waits:\n%s", logs.String())
	}
	p.held[free.Addr] = true
	looks := net.looks
	net.who = nil
	if got := p.claim(context.Background(), []PlanVIP{free}); len(got) != 1 || net.looks != looks {
		t.Errorf("a VIP held: claimed %v, with %d more looks", got, net.looks-looks)
	}
}
