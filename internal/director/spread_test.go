package director

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
)

// TestChecksAreSpreadAndMoveLittle: every director of three gets a share of
// the checks, and one going moves only the checks that were its own.
func TestChecksAreSpreadAndMoveLittle(t *testing.T) {
	three, two := []string{"d1", "d2", "d3"}, []string{"d1", "d2"}
	share := map[string]int{}
	moved := 0
	for i := range 300 {
		key := fmt.Sprintf("http:/ 10.0.0.%d:80", i)
		by := checker(key, three)
		share[by]++
		if after := checker(key, two); by != "d3" && after != by {
			moved++
		}
	}
	for _, d := range three {
		if share[d] < 60 {
			t.Errorf("%s checks %d of 300: %v", d, share[d], share)
		}
	}
	if moved != 0 {
		t.Errorf("d3 going moved %d checks that were not its own", moved)
	}
}

// TestAGivenCheckIsTheLeadersUntilItsResultComes: a check given to another
// director stays the leader's own until that director's result has come, then
// goes by it while it is fresh, and back to the leader's when it is not.
func TestAGivenCheckIsTheLeadersUntilItsResultComes(t *testing.T) {
	s := &session{n: &node{cfg: Config{Name: "d1"}}}
	p := &plane{s: s, probers: map[string]*prober{}, remote: map[string]remoteCheck{}, checkBy: map[string]string{}}
	checkers := []string{"d1", "d2"}
	key := ""
	for i := 0; key == ""; i++ {
		if k := fmt.Sprintf("none 10.0.0.%d:80", i); checker(k, checkers) == "d2" {
			key = k
		}
	}
	target := netip.MustParseAddrPort("10.0.0.1:80")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	local, given := map[string]bool{}, map[string]map[string]api.Probe{}
	if !p.check(ctx, key, "none", target, true, checkers, local, given) || !local[key] || given["d2"][key].Key != key || !given["d2"][key].Healthy {
		t.Fatalf("before d2's result: healthy as the state says, by the leader's own check, given to d2: local %v, given %v", local, given)
	}
	if out := p.report("d2", map[string]api.ProbeResult{key: {Healthy: false, Seen: true}}); len(out) != 0 {
		t.Errorf("report gave d2 %v before the step put anything in given", out)
	}
	p.given = given
	if out := p.report("d2", map[string]api.ProbeResult{key: {Healthy: false, Seen: true}}); len(out) != 1 || out[0].Key != key {
		t.Errorf("d2 is given %v; want the one check", out)
	}
	local, given = map[string]bool{}, map[string]map[string]api.Probe{}
	if p.check(ctx, key, "none", target, true, checkers, local, given) || local[key] || p.checkBy[key] != "d2" {
		t.Errorf("with d2's fresh result: want d2's fail, not the leader's own check (local %v, by %q)", local, p.checkBy[key])
	}
	r := p.remote[key]
	r.at = time.Now().Add(-2 * liveFor)
	p.remote[key] = r
	local = map[string]bool{}
	if p.check(ctx, key, "none", target, true, checkers, local, given); !local[key] || p.checkBy[key] != "d1" {
		t.Errorf("with d2's result stale: want the leader's own check again (local %v, by %q)", local, p.checkBy[key])
	}
}
