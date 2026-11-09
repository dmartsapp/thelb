//go:build linux

package director

import (
	"crypto/tls"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/status"
	"github.com/dmartsapp/thelb/internal/wire"
)

// lab is directors on the loopback interface - Linux has all of 127.0.0.0/8
// there - each with its own address, the control VIP at 127.0.0.10, and one
// control port for them all, as on a real network.
type lab struct {
	port int
	macs *sync.Map
}

func newLab(t *testing.T) *lab { return &lab{port: freePort(t, "127.0.0.10"), macs: &sync.Map{}} }

func (l *lab) director(t *testing.T, i int) *world {
	ip := fmt.Sprintf("127.0.0.%d", 10+i)
	w := newWorld(t, fmt.Sprintf("lb-%d", i), netip.MustParsePrefix(ip+"/8"), fmt.Sprintf("52:54:00:00:00:%02x", i), fmt.Sprintf("%s:%d", ip, l.port), ip, fmt.Sprintf("127.0.0.10:%d", l.port), l.macs)
	w.cfg.ControlVIP = netip.MustParseAddr("127.0.0.10")
	w.cfg.ControlPort = uint16(l.port)
	w.cfg.Bootstrap = i == 1
	return w
}

// copySecrets gives w the secret files of from, as the user would.
func copySecrets(t *testing.T, from, to *world) {
	t.Helper()
	for _, f := range [][2]string{{from.cfg.SecretFile, to.cfg.SecretFile}, {from.cfg.BackendSecretFile, to.cfg.BackendSecretFile}} {
		b, err := os.ReadFile(f[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f[1], b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func nodeRow(st status.Status, name string) *status.Node {
	for i := range st.Nodes {
		if st.Nodes[i].Node == name {
			return &st.Nodes[i]
		}
	}
	return nil
}

func pending(st status.Status, name string) *status.Pending {
	for i := range st.Pending {
		if st.Pending[i].Pending == name {
			return &st.Pending[i]
		}
	}
	return nil
}

// allUp is whether st shows exactly names, all up, one of them leading.
func allUp(st status.Status, names ...string) bool {
	if len(st.Nodes) != len(names) || st.Cluster == nil || st.Cluster.Leader == "" {
		return false
	}
	for _, name := range names {
		n := nodeRow(st, name)
		if n == nil || n.State != "up" {
			return false
		}
	}
	return true
}

func vipLeader(t *testing.T, w *world) string {
	t.Helper()
	var cl wire.Cluster
	c, err := tls.Dial("tcp", w.net.vip, wire.JoinConfig(w.cfg.ControlVIP, &cl))
	if err != nil {
		return ""
	}
	_ = c.Close()
	return cl.Leader
}

func admin(w *world, op string, names ...string) ([]string, error) {
	resp, err := localsock.Call(w.run, localsock.Request{Op: localsock.OpChange, Change: &api.Change{Op: op, Names: names}})
	if err != nil || resp.Result == nil {
		return nil, err
	}
	return resp.Result.Names, nil
}

func TestACluster(t *testing.T) {
	l := newLab(t)
	d1 := l.director(t, 1)
	d1.start(t)

	// Two more, approved at once on the first.
	d2, d3 := l.director(t, 2), l.director(t, 3)
	for _, d := range []*world{d2, d3} {
		copySecrets(t, d1, d)
		d.launch(t)
	}
	eventually(t, "two nodes pending on the leader", func() bool { return len(d1.status(t).Pending) == 2 })
	st := d1.status(t)
	for _, d := range []*world{d2, d3} {
		own := d.status(t)
		p := pending(st, d.cfg.Name)
		if p == nil || own.Nodes[0].State != "waiting for approval" || own.Nodes[0].NodeFingerprint != p.NodeFingerprint || p.MAC != d.local.MAC || p.Address != d.local.Prefix.Addr().String() {
			t.Fatalf("pending %+v; the node's own view %+v", p, own.Nodes)
		}
		if own.Cluster == nil || own.Cluster.Name != "prod" {
			t.Errorf("a pending node does not know its cluster: %+v", own.Cluster)
		}
	}
	if _, err := admin(d2, "approve", "lb-3"); err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Errorf("a pending node approved another: %v", err)
	}
	if got, err := admin(d1, "approve", "lb-2", "lb-3"); err != nil || len(got) != 2 {
		t.Fatalf("approving: %v %v", got, err)
	}
	d2.waitReady(t)
	d3.waitReady(t)
	eventually(t, "three directors up", func() bool { return allUp(d1.status(t), "lb-1", "lb-2", "lb-3") })
	eventually(t, "a follower to see the same", func() bool { return allUp(d2.status(t), "lb-1", "lb-2", "lb-3") })
	if f, l := d2.status(t).Cluster, d1.status(t).Cluster; f.Leader != "lb-1" || *f.Term != *l.Term || f.ID != l.ID {
		t.Errorf("the follower's cluster %+v, the leader's %+v", f, l)
	}
	if n := nodeRow(d1.status(t), "lb-2"); n.LastCheckIn == nil {
		t.Errorf("no check-in for lb-2: %+v", n)
	}
	if !strings.Contains(d1.logs.String(), "OK director joined node=lb-2 approved_by=lb-1") || !strings.Contains(d1.logs.String(), "OK director promoted to voter node=lb-3") {
		t.Errorf("the leader's log:\n%s", d1.logs)
	}

	// A fourth, approved through a follower: the change goes to the leader.
	d4 := l.director(t, 4)
	copySecrets(t, d1, d4)
	d4.launch(t)
	eventually(t, "lb-4 pending", func() bool { return pending(d2.status(t), "lb-4") != nil })
	if _, err := admin(d2, "approve", "lb-4"); err != nil {
		t.Fatalf("approving through a follower: %v", err)
	}
	d4.waitReady(t)
	if !strings.Contains(d1.logs.String(), "OK node approved node=lb-4 by=lb-2") {
		t.Errorf("the approval is not lb-2's:\n%s", d1.logs)
	}
	eventually(t, "four directors up", func() bool { return allUp(d1.status(t), "lb-1", "lb-2", "lb-3", "lb-4") })

	// A planned stop of the leader hands leadership over; the control VIP follows.
	if err := d1.halt(t); err != nil {
		t.Fatalf("stopping the leader: %v", err)
	}
	if !strings.Contains(d1.logs.String(), "OK leadership handed over to=lb-") {
		t.Errorf("no hand-over:\n%s", d1.logs)
	}
	var leader string
	eventually(t, "a new leader on the control VIP", func() bool {
		leader = vipLeader(t, d2)
		return leader != "" && leader != "lb-1"
	})
	eventually(t, "the others to agree", func() bool {
		st := d2.status(t)
		return st.Cluster.Leader == leader && nodeRow(st, "lb-1") != nil && nodeRow(st, "lb-1").State == "stopped"
	})
	if !strings.Contains(d1.logs.String(), "OK out of the vote") {
		t.Errorf("the stopped leader did not leave the vote:\n%s", d1.logs)
	}

	// The old leader comes back - with --bootstrap, as it was started - finds the
	// cluster, and rejoins without approval: the cluster remembers it.
	d1.cfg.Bootstrap = true
	d1.logs = &syncBuffer{}
	d1.start(t)
	if !strings.Contains(d1.logs.String(), "OK joined the cluster cluster=prod") || strings.Contains(d1.logs.String(), "waiting for approval") {
		t.Errorf("the restarted director did not rejoin by itself:\n%s", d1.logs)
	}
	eventually(t, "four up again", func() bool { return allUp(d2.status(t), "lb-1", "lb-2", "lb-3", "lb-4") })
	var lw *world
	for _, d := range []*world{d2, d3, d4} {
		if d.cfg.Name == leader {
			lw = d
		}
	}
	// The new leader took lb-1's identity out of Raft when it stopped: nothing
	// is left to remove when it rejoins.
	if !strings.Contains(lw.logs.String(), "OK director stopped: out of the vote until it starts again node=lb-1") || strings.Contains(lw.logs.String(), "OK old member identity removed node=lb-1") {
		t.Errorf("the leader's log:\n%s", lw.logs)
	}
	if !strings.Contains(lw.logs.String(), "OK director rejoined: approved before node=lb-1") {
		t.Errorf("the leader's log:\n%s", lw.logs)
	}

	// Removing a director: it leaves Raft, and stops.
	victim := d4
	if leader == "lb-4" {
		victim = d3
	}
	if _, err := admin(d1, "remove", victim.cfg.Name); err != nil {
		t.Fatalf("removing: %v", err)
	}
	select {
	case err := <-victim.errc:
		victim.errc <- err
		if err == nil || !strings.Contains(err.Error(), "removed from the cluster") {
			t.Errorf("the removed director ended with %v", err)
		}
	case <-timeout():
		t.Fatalf("the removed director is still running\n%s", victim.logs)
	}
	eventually(t, "three left", func() bool { return len(d1.status(t).Nodes) == 3 && len(d1.status(t).Certificates) == 4 })

	// Refusing a node: it is told so, and stops.
	d5 := l.director(t, 5)
	copySecrets(t, d1, d5)
	d5.launch(t)
	eventually(t, "lb-5 pending", func() bool { return pending(d1.status(t), "lb-5") != nil })
	if _, err := admin(d1, "refuse", "lb-5"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-d5.errc:
		d5.errc <- err
		if err == nil || !strings.Contains(err.Error(), "refused this director: refused by lb-1") {
			t.Errorf("the refused director ended with %v", err)
		}
	case <-timeout():
		t.Fatalf("the refused director is still running\n%s", d5.logs)
	}
	eventually(t, "the refusal on every director", func() bool {
		for _, d := range []*world{d1, d2, d3} {
			if d == victim {
				continue
			}
			if st := d.status(t); len(st.Pending) != 0 || nodeRow(st, "lb-5") != nil {
				return false
			}
		}
		return true
	})

	// No secret in any log.
	raw, _ := os.ReadFile(d1.cfg.SecretFile)
	key := strings.TrimSpace(strings.TrimPrefix(string(raw), "thelb1:"))
	for _, d := range []*world{d1, d2, d3, d4, d5} {
		if strings.Contains(d.logs.String(), key) {
			t.Errorf("the secret is in %s's log", d.cfg.Name)
		}
	}
}

func timeout() <-chan time.Time { return time.After(20 * time.Second) }

// TestTwoDirectorsSurviveEitherOnesPlannedStop: with two voters, a planned stop
// takes the stopping director out of the vote, so the one left keeps - or wins -
// leadership alone; and the stopped one rejoins without approval.
func TestTwoDirectorsSurviveEitherOnesPlannedStop(t *testing.T) {
	l := newLab(t)
	d1, d2 := l.director(t, 1), l.director(t, 2)
	d1.start(t)
	copySecrets(t, d1, d2)
	d2.launch(t)
	eventually(t, "lb-2 pending", func() bool { return pending(d1.status(t), "lb-2") != nil })
	if _, err := admin(d1, "approve", "lb-2"); err != nil {
		t.Fatal(err)
	}
	d2.waitReady(t)
	eventually(t, "two voters up", func() bool { return allUp(d1.status(t), "lb-1", "lb-2") })

	// The follower stops: the leader keeps its leadership.
	if err := d2.halt(t); err != nil {
		t.Fatalf("stopping the follower: %v", err)
	}
	if !strings.Contains(d2.logs.String(), "OK out of the vote") {
		t.Errorf("the follower did not leave the vote:\n%s", d2.logs)
	}
	time.Sleep(3 * time.Second) // longer than a leader takes to find it has no majority
	st := d1.status(t)
	if st.Cluster.Leader != "lb-1" || vipLeader(t, d1) != "lb-1" || strings.Contains(d1.logs.String(), "stepping down") {
		t.Fatalf("the leader, alone: leader %q\n%s", st.Cluster.Leader, d1.logs)
	}
	if n := nodeRow(st, "lb-2"); n == nil || n.State != "stopped" {
		t.Errorf("lb-2 in status: %+v", n)
	}

	// It comes back by itself; then the leader stops, and lb-2 leads alone.
	d2.cfg.Bootstrap = true
	d2.logs = &syncBuffer{}
	d2.start(t)
	if strings.Contains(d2.logs.String(), "waiting for approval") {
		t.Errorf("lb-2 waited for approval again:\n%s", d2.logs)
	}
	eventually(t, "two voters up again", func() bool { return allUp(d1.status(t), "lb-1", "lb-2") })
	if err := d1.halt(t); err != nil {
		t.Fatalf("stopping the leader: %v", err)
	}
	if !strings.Contains(d1.logs.String(), "OK leadership handed over to=lb-2") || !strings.Contains(d1.logs.String(), "OK out of the vote") {
		t.Errorf("the leader's stop:\n%s", d1.logs)
	}
	eventually(t, "lb-2 leading", func() bool { return vipLeader(t, d2) == "lb-2" })
	time.Sleep(3 * time.Second)
	if st := d2.status(t); st.Cluster.Leader != "lb-2" || strings.Contains(d2.logs.String(), "stepping down") {
		t.Fatalf("lb-2, alone: leader %q\n%s", st.Cluster.Leader, d2.logs)
	}
}
