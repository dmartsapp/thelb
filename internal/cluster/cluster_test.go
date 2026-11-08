package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

var t0 = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

type applier struct {
	t     *testing.T
	f     *FSM
	index uint64
}

func (a *applier) apply(typ, by string, at time.Time, data any) error {
	a.t.Helper()
	b, err := NewEntry(typ, by, at, data)
	if err != nil {
		a.t.Fatal(err)
	}
	a.index++
	r := a.f.Apply(&raft.Log{Index: a.index, Type: raft.LogCommand, Data: b})
	if r == nil {
		return nil
	}
	return r.(error)
}

func claim(name, addr string) Claim {
	return Claim{Name: name, Role: "director", Address: addr, MAC: "52:54:00:00:00:" + addr[len(addr)-2:], Fingerprint: "fp-" + name, Hostname: name, Version: "0.1.0"}
}

func created(t *testing.T) *applier {
	a := &applier{t: t, f: New()}
	err := a.apply(TypeCreate, "bootstrap", t0, Create{
		Info: Info{Name: "prod", ID: "7f3c91a2", Created: t0, CACert: []byte{1}, CAKey: []byte{2}},
		Node: Node{Claim: claim("lb-1", "10.0.0.11"), ApprovedBy: "bootstrap", ApprovedAt: t0, RaftID: "lb-1-aaaa", RaftAddr: "10.0.0.11:7443"},
		Cert: Cert{Name: "lb-1", Kind: "director", Serial: "a1", NotAfter: t0.Add(24 * time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestApproveAdmitRemove(t *testing.T) {
	a := created(t)
	c := claim("lb-2", "10.0.0.12")
	if err := a.apply(TypePending, "lb-1", t0.Add(time.Second), Pending{Claim: c, Since: t0.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	// Asking again keeps the time it first asked.
	if err := a.apply(TypePending, "lb-1", t0.Add(time.Minute), Pending{Claim: c, Since: t0.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	a.f.Read(func(s *State) {
		if p := s.Pending["lb-2"]; p == nil || !p.Since.Equal(t0.Add(time.Second)) {
			t.Errorf("pending %+v", p)
		}
	})
	if err := a.apply(TypeAdmit, "lb-1", t0, Admit{Claim: c, RaftID: "lb-2-bbbb"}); err == nil {
		t.Error("a pending node was admitted before it was approved")
	}
	if err := a.apply(TypeApprove, "lb-3", t0.Add(2*time.Minute), Names{Names: []string{"lb-2"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.apply(TypeAdmit, "lb-1", t0.Add(3*time.Minute), Admit{Claim: c, RaftID: "lb-2-bbbb", RaftAddr: "10.0.0.12:7443", Cert: Cert{Name: "lb-2", Kind: "director", Serial: "b1", NotAfter: t0.Add(25 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	a.f.Read(func(s *State) {
		n := s.Nodes["lb-2"]
		if n == nil || n.ApprovedBy != "lb-3" || n.RaftID != "lb-2-bbbb" || n.AdmitIndex != a.index || len(s.Pending) != 0 {
			t.Errorf("node %+v, pending %v", n, s.Pending)
		}
		if s.NodeByRaftID("lb-2-bbbb") != n {
			t.Error("not found by its member identity")
		}
	})

	// A restart: a new identity, the old certificate revoked.
	if err := a.apply(TypeAdmit, "lb-1", t0.Add(4*time.Minute), Admit{Claim: c, RaftID: "lb-2-cccc", RaftAddr: "10.0.0.12:7443", Cert: Cert{Name: "lb-2", Kind: "director", Serial: "b2", NotAfter: t0.Add(26 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	a.f.Read(func(s *State) {
		if s.Nodes["lb-2"].RaftID != "lb-2-cccc" || s.Certs["lb-2"].Serial != "b2" || s.Revoked["b1"].IsZero() {
			t.Errorf("after a restart: %+v %+v %v", s.Nodes["lb-2"], s.Certs["lb-2"], s.Revoked)
		}
	})
	// The same name from another machine is not admitted.
	other := c
	other.Fingerprint = "fp-clone"
	if err := a.apply(TypeAdmit, "lb-1", t0, Admit{Claim: other, RaftID: "lb-2-dddd"}); err == nil {
		t.Error("another machine was admitted under an approved name")
	}

	if err := a.apply(TypeRemove, "lb-1", t0.Add(5*time.Minute), Name{Name: "lb-2"}); err != nil {
		t.Fatal(err)
	}
	a.f.Read(func(s *State) {
		if s.Nodes["lb-2"] != nil || s.Certs["lb-2"] != nil || s.Revoked["b2"].IsZero() {
			t.Errorf("after removal: %+v %+v %v", s.Nodes["lb-2"], s.Certs["lb-2"], s.Revoked)
		}
	})
	// Revoked serials are forgotten once they would have expired.
	if err := a.apply(TypeLeader, "lb-1", t0.Add(27*time.Hour), Leader{Name: "lb-1", Term: 3}); err != nil {
		t.Fatal(err)
	}
	a.f.Read(func(s *State) {
		if len(s.Revoked) != 0 {
			t.Errorf("revoked %v", s.Revoked)
		}
	})
}

func TestRefusalIsRememberedForAnHour(t *testing.T) {
	a := created(t)
	c := claim("lb-9", "10.0.0.19")
	_ = a.apply(TypePending, "lb-1", t0, Pending{Claim: c, Since: t0})
	if err := a.apply(TypeRefuse, "lb-1", t0, Name{Name: "lb-9"}); err != nil {
		t.Fatal(err)
	}
	a.f.Read(func(s *State) {
		if r := s.Refused["lb-9"]; r == nil || r.By != "lb-1" || len(s.Pending) != 0 {
			t.Errorf("refused %+v", r)
		}
	})
	if err := a.apply(TypeRefuse, "lb-1", t0, Name{Name: "lb-9"}); err == nil {
		t.Error("refused a node that is not pending")
	}
	_ = a.apply(TypeLeader, "lb-1", t0.Add(RefusalMemory+time.Second), Leader{Name: "lb-1", Term: 2})
	a.f.Read(func(s *State) {
		if len(s.Refused) != 0 {
			t.Errorf("a refusal outlived an hour: %v", s.Refused)
		}
	})
}

func TestAnOlderTermDoesNotReplaceTheLeader(t *testing.T) {
	a := created(t)
	_ = a.apply(TypeLeader, "lb-2", t0, Leader{Name: "lb-2", Term: 5})
	if err := a.apply(TypeLeader, "lb-1", t0, Leader{Name: "lb-1", Term: 4}); err == nil {
		t.Error("a deposed leader's entry took effect")
	}
	a.f.Read(func(s *State) {
		if s.Leader.Name != "lb-2" {
			t.Errorf("leader %+v", s.Leader)
		}
	})
}

func TestAnUnknownEntryStopsTheDirector(t *testing.T) {
	for name, entry := range map[string]Entry{
		"type":    {V: EntryVersion, Type: "service.add", At: t0},
		"version": {V: EntryVersion + 1, Type: TypeLeader, At: t0},
	} {
		t.Run(name, func(t *testing.T) {
			f := New()
			var stopped error
			f.OnFatal = func(err error) { stopped = err }
			b, _ := json.Marshal(entry)
			r := f.Apply(&raft.Log{Index: 7, Type: raft.LogCommand, Data: b})
			err, _ := r.(error)
			if !errors.Is(err, ErrUnknownEntry) || !errors.Is(stopped, ErrUnknownEntry) {
				t.Errorf("result %v, stopped %v", r, stopped)
			}
		})
	}
}

type sink struct {
	bytes.Buffer
	cancelled bool
}

func (s *sink) ID() string    { return "1" }
func (s *sink) Cancel() error { s.cancelled = true; return nil }
func (s *sink) Close() error  { return nil }

func TestSnapshotAndRestore(t *testing.T) {
	a := created(t)
	_ = a.apply(TypePending, "lb-1", t0, Pending{Claim: claim("lb-2", "10.0.0.12"), Since: t0})
	snap, err := a.f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var out sink
	if err := snap.Persist(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"ca_key"`) {
		t.Error("the snapshot lacks the CA's key: a new director could not sign")
	}
	g := New()
	changed := g.Changed()
	if err := g.Restore(io.NopCloser(&out.Buffer)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Error("a restore did not signal a change")
	}
	g.Read(func(s *State) {
		if s.Cluster == nil || s.Cluster.ID != "7f3c91a2" || s.Nodes["lb-1"] == nil || s.Pending["lb-2"] == nil || s.Refused == nil {
			t.Errorf("restored %+v", s)
		}
	})
}
