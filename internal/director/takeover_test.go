package director

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/dmartsapp/thelb/internal/cluster"
)

// TestStandAloneOnlyWhenTheOtherIsGone: the last of two directors stands alone
// when the other's theLB is not running, or its machine does not answer - and
// never while a theLB answers on the control VIP, another machine holds it, the
// other took it after this director saw it free (it decided first, alive), this
// director is cut off itself, or the other answers on the network while its
// port drops connections (a partition: it may be leading).
func TestStandAloneOnlyWhenTheOtherIsGone(t *testing.T) {
	refused := fmt.Errorf("dial tcp 192.168.1.92:8080: %w", syscall.ECONNREFUSED)
	timeout := errors.New("dial tcp 192.168.1.92:8080: i/o timeout")
	gone := takeoverFacts{other: "linux", otherMAC: "02:11:32:24:ac:11", otherLed: true, uplink: true, portErr: refused}
	for _, c := range []struct {
		name string
		edit func(*takeoverFacts)
		want bool
		why  string
	}{
		{"its theLB was killed: the port refuses, the VIP is still on its interface", func(f *takeoverFacts) { f.vipHolder = "02:11:32:24:AC:11" }, true, "refuses connections"},
		{"its machine is down: nothing answers", func(f *takeoverFacts) { f.portErr = timeout }, true, "does not answer on the network"},
		{"a firewall drops its port: a partition", func(f *takeoverFacts) { f.portErr, f.answers = timeout, "02:11:32:24:ac:11" }, false, "a partition, not a crash"},
		{"its port accepts", func(f *takeoverFacts) { f.portErr = nil }, false, "accepts connections"},
		{"a theLB answers on the VIP", func(f *takeoverFacts) { f.vipTheLB = true }, false, "a theLB answers on the control VIP"},
		{"another machine holds the VIP", func(f *takeoverFacts) { f.vipHolder = "52:54:00:aa:bb:cc" }, false, "52:54:00:aa:bb:cc holds the control VIP"},
		{"this director is cut off", func(f *takeoverFacts) { f.uplink, f.uplinkWhy = false, "its gateway 192.168.1.1 does not answer" }, false, "does not reach its network: its gateway"},
		{"its machine is down, the VIP free since", func(f *takeoverFacts) { f.portErr, f.vipWasFree = timeout, true }, true, "does not answer on the network"},
		{"a firewall rejects its port, and it took the VIP this director saw free: it decided first", func(f *takeoverFacts) {
			f.vipHolder, f.vipWasFree = "02:11:32:24:ac:11", true
		}, false, "took the control VIP after this director saw it free"},
		{"it holds the VIP, but this director led last", func(f *takeoverFacts) { f.vipHolder, f.otherLed = "02:11:32:24:ac:11", false }, false, "was not the leader this director saw last"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := gone
			c.edit(&f)
			got, why := standAlone(f)
			if got != c.want || !strings.Contains(why, c.why) {
				t.Errorf("standAlone = %v, %q; want %v, %q", got, why, c.want, c.why)
			}
		})
	}
}

func TestTakeoverIsAnError(t *testing.T) {
	var err error = &takeover{why: "linux does not answer on the network"}
	var tk *takeover
	if !errors.As(fmt.Errorf("session: %w", err), &tk) || tk.why == "" || errors.Is(err, errRejoin) {
		t.Errorf("a takeover is not told apart: %v", err)
	}
}

// TestReplayKeepsTheLogsTail: a director standing alone keeps the entries its
// log holds past what it applied - the leader may have confirmed them before it
// went - as hashicorp/raft's RecoverCluster does, from the index the state
// itself says it applied, skipping what is not a command.
func TestReplayKeepsTheLogsTail(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	entry := func(index uint64, typ string, data any) *raft.Log {
		b, err := cluster.NewEntry(typ, "lb-1", at, data)
		if err != nil {
			t.Fatal(err)
		}
		return &raft.Log{Index: index, Term: 2, Type: raft.LogCommand, Data: b}
	}
	pending := func(name string) cluster.Pending {
		return cluster.Pending{Claim: cluster.Claim{Name: name, Role: "director", Address: "10.0.0.12", MAC: "52:54:00:00:00:12", Fingerprint: "fp-" + name}, Since: at}
	}
	log := []*raft.Log{
		entry(1, cluster.TypeCreate, cluster.Create{
			Info: cluster.Info{Name: "prod", ID: "7f3c91a2", Created: at, CACert: []byte{1}, CAKey: []byte{2}},
			Node: cluster.Node{Claim: cluster.Claim{Name: "lb-1", Role: "director", Address: "10.0.0.11", MAC: "52:54:00:00:00:11"}, RaftID: "lb-1-aaaa", RaftAddr: "10.0.0.11:7443"},
			Cert: cluster.Cert{Name: "lb-1", Kind: "director", Serial: "a1", NotAfter: at.Add(24 * time.Hour)},
		}),
		entry(2, cluster.TypePending, pending("lb-2")),
		entry(3, cluster.TypePending, pending("lb-3")),
		{Index: 4, Term: 2, Type: raft.LogNoop},
		entry(5, cluster.TypePending, pending("lb-4")),
	}
	applied := cluster.New()
	for _, l := range log[:2] {
		applied.Apply(l)
	}
	state, err := applied.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		from    int    // the first entry the log still holds
		applied uint64 // what Raft says it applied, for a state that does not say
		want    []string
		n       int
	}{
		{"the whole log", 0, 99, []string{"lb-2", "lb-3", "lb-4"}, 2},
		{"a log compacted past what was applied", 2, 99, []string{"lb-2", "lb-3", "lb-4"}, 2},
		{"a log compacted further: its first entries are in the state", 3, 99, []string{"lb-2", "lb-4"}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := raft.NewInmemStore()
			if err := store.StoreLogs(log[c.from:]); err != nil {
				t.Fatal(err)
			}
			out, last, n, err := replay(state, store, c.applied)
			if err != nil {
				t.Fatal(err)
			}
			got := cluster.New()
			if err := got.Restore(nopCloser{strings.NewReader(string(out))}); err != nil {
				t.Fatal(err)
			}
			var names []string
			var idx uint64
			got.Read(func(st *cluster.State) {
				names, idx = cluster.Sorted(st.Pending), st.Applied
			})
			if fmt.Sprint(names) != fmt.Sprint(c.want) || n != c.n || last != 5 || idx != 5 && n > 0 {
				t.Errorf("replay: pending %v, %d entries, last %d, applied %d; want %v, %d, 5, 5", names, n, last, idx, c.want, c.n)
			}
		})
	}
	// A state that does not say what it applied - a snapshot from an older
	// theLB - starts after what Raft says.
	old := strings.Replace(string(state), `"applied":2`, `"applied":0`, 1)
	store := raft.NewInmemStore()
	if err := store.StoreLogs(log); err != nil {
		t.Fatal(err)
	}
	if _, _, n, err := replay([]byte(old), store, 3); err != nil || n != 1 {
		t.Errorf("replay of an older state from Raft's applied index: %d entries, %v; want 1", n, err)
	}
}

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }

// TestTwoLeadersLeaveOne: a leader that finds the control VIP answered by the
// director its takeover left behind, alive, stands down only when its member
// identity sorts later - the other, deciding the same, keeps leading; a theLB
// killed, its VIP left on its interface, and any other machine are answered by
// announcing the VIP again.
func TestTwoLeadersLeaveOne(t *testing.T) {
	cp := &peer{name: "cp", mac: "52:54:00:aa:bb:cc", raftID: "cp-7c2e01aa", addr: "192.168.1.136:8080"}
	for _, c := range []struct {
		name  string
		p     *peer
		left  bool
		alive bool
		self  string
		down  bool
		why   string
	}{
		{"the one left behind leads too, and sorts first", cp, true, true, "linux-0f3d2b11", true, "this director starts over"},
		{"the one left behind leads too, and sorts later", cp, true, true, "alpha-0a99ffee", false, "cp stands down"},
		{"the one left behind was killed, its VIP left", cp, true, false, "linux-9999", false, "what a theLB killed leaves"},
		{"a member's machine answers, its theLB running", cp, false, true, "linux-9999", false, "with its theLB running"},
		{"some other machine", nil, false, false, "linux-9999", false, "an address conflict"},
	} {
		t.Run(c.name, func(t *testing.T) {
			down, why := vipConflict("52:54:00:aa:bb:cc", c.p, c.left, c.alive, c.self)
			if down != c.down || !strings.Contains(why, c.why) {
				t.Errorf("vipConflict = %v, %q; want %v, %q", down, why, c.down, c.why)
			}
		})
	}
}
