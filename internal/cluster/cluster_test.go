package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
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
		"type":    {V: EntryVersion, Type: "k8s.allocate", At: t0},
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

func TestPoolsServicesOffersAssignments(t *testing.T) {
	a := created(t)
	web := Claim{Name: "web-1", Role: "backend", Address: "10.0.0.21", MAC: "52:54:00:00:00:21", Fingerprint: "fp-web-1"}
	_ = a.apply(TypePending, "lb-1", t0, Pending{Claim: web, Since: t0})
	_ = a.apply(TypeApprove, "lb-1", t0, Names{Names: []string{"web-1"}})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(a.apply(TypePoolAdd, "lb-1", t0, Pool{Name: "external", First: "10.10.1.20", Bits: 24, Count: 10}))
	if err := a.apply(TypePoolAdd, "lb-1", t0, Pool{Name: "external", First: "10.10.2.20", Bits: 24, Count: 1}); err == nil {
		t.Error("two pools with one name")
	}
	must(a.apply(TypeServiceAdd, "lb-1", t0, Service{Name: "nginx", Pool: "external", VIP: "10.10.1.20", Proto: "tcp", Port: 80, Scheduler: "wrr"}))
	if err := a.apply(TypeServiceAdd, "lb-1", t0, Service{Name: "other", Pool: "external", VIP: "10.10.1.20", Proto: "tcp", Port: 80, Scheduler: "wrr"}); err == nil {
		t.Error("two services on one VIP, protocol and port")
	}
	must(a.apply(TypeServiceAdd, "lb-1", t0, Service{Name: "dns", Pool: "external", VIP: "10.10.1.20", Proto: "udp", Port: 53, Scheduler: "wrr"}))
	if err := a.apply(TypeOffers, "web-1", t0, SetOffers{Node: "lb-1", Offers: []Offer{{Proto: "tcp", Port: 80}}}); err == nil {
		t.Error("a director made offers")
	}
	must(a.apply(TypeOffers, "web-1", t0, SetOffers{Node: "web-1", Offers: []Offer{{Proto: "tcp", Port: 80, Check: "tcp"}, {Proto: "udp", Port: 53, Check: "none"}}}))
	must(a.apply(TypeAssign, "lb-1", t0, Assign{Assignments: []Assignment{{Service: "nginx", Node: "web-1", Proto: "tcp", Port: 80, Weight: 3}, {Service: "dns", Node: "web-1", Proto: "udp", Port: 53, Weight: 1}}}))
	must(a.apply(TypeHealth, "lb-1", t0, Health{Service: "nginx", Node: "web-1", Healthy: true}))
	// A new weight keeps the health it had.
	must(a.apply(TypeAssign, "lb-1", t0, Assign{Assignments: []Assignment{{Service: "nginx", Node: "web-1", Proto: "tcp", Port: 80, Weight: 5}}}))
	a.f.Read(func(s *State) {
		x := s.Assignments[AssignmentKey("nginx", "web-1")]
		if x == nil || x.Weight != 5 || !x.Healthy || x.By != "lb-1" {
			t.Errorf("assignment %+v", x)
		}
		if p := s.Pools["external"]; !p.Contains(netip.MustParseAddr("10.10.1.29")) || p.Contains(netip.MustParseAddr("10.10.1.30")) {
			t.Error("the pool's range")
		}
	})
	if err := a.apply(TypePoolRemove, "lb-1", t0, Name{Name: "external"}); err == nil {
		t.Error("a pool in use was removed")
	}
	// Withdrawing an offer ends its assignment.
	must(a.apply(TypeOffers, "web-1", t0, SetOffers{Node: "web-1", Offers: []Offer{{Proto: "tcp", Port: 80, Check: "tcp"}}}))
	a.f.Read(func(s *State) {
		if s.Assignments[AssignmentKey("dns", "web-1")] != nil || s.Assignments[AssignmentKey("nginx", "web-1")] == nil {
			t.Errorf("after withdrawing udp/53: %v", s.Assignments)
		}
	})
	must(a.apply(TypeServiceRemove, "lb-1", t0, Name{Name: "nginx"}))
	must(a.apply(TypeServiceRemove, "lb-1", t0, Name{Name: "dns"}))
	must(a.apply(TypePoolRemove, "lb-1", t0, Name{Name: "external"}))
	// Removing a backend removes its offers and assignments.
	must(a.apply(TypePoolAdd, "lb-1", t0, Pool{Name: "p", First: "10.10.1.20", Bits: 24, Count: 1}))
	must(a.apply(TypeServiceAdd, "lb-1", t0, Service{Name: "s", Pool: "p", VIP: "10.10.1.20", Proto: "tcp", Port: 80}))
	must(a.apply(TypeAssign, "lb-1", t0, Assign{Assignments: []Assignment{{Service: "s", Node: "web-1", Proto: "tcp", Port: 80, Weight: 1}}}))
	must(a.apply(TypeRemove, "lb-1", t0, Name{Name: "web-1"}))
	a.f.Read(func(s *State) {
		if len(s.Assignments) != 0 || len(s.Offers) != 0 {
			t.Errorf("after removing the backend: %v %v", s.Assignments, s.Offers)
		}
	})
}

// TestOffersAddedOneByOne: a backend's offers, each by name - the cluster's
// record of them, which assignments follow.
func TestOffersAddedOneByOne(t *testing.T) {
	a := created(t)
	web := Claim{Name: "web-1", Role: "backend", Address: "10.0.0.21", MAC: "52:54:00:00:00:21", Fingerprint: "fp-web-1"}
	_ = a.apply(TypePending, "lb-1", t0, Pending{Claim: web, Since: t0})
	_ = a.apply(TypeApprove, "lb-1", t0, Names{Names: []string{"web-1"}})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	t1 := t0.Add(time.Minute)
	must(a.apply(TypeOfferAdd, "web-1", t0, OfferAdd{Node: "web-1", Offer: Offer{Name: "shop", Proto: "tcp", Port: 8080, Check: "tcp"}}))
	must(a.apply(TypeOfferAdd, "web-1", t0, OfferAdd{Node: "web-1", Offer: Offer{Name: "dns", Proto: "udp", Port: 53, Check: "none"}}))
	if err := a.apply(TypeOfferAdd, "web-1", t0, OfferAdd{Node: "web-1", Offer: Offer{Name: "other", Proto: "tcp", Port: 8080, Check: "tcp"}}); err == nil {
		t.Error("two offers at one port")
	}
	if err := a.apply(TypeOfferAdd, "lb-1", t0, OfferAdd{Node: "lb-1", Offer: Offer{Name: "x", Proto: "tcp", Port: 1, Check: "tcp"}}); err == nil {
		t.Error("a director made an offer")
	}
	if err := a.apply(TypeOfferAdd, "web-1", t0, OfferAdd{Node: "web-1", Offer: Offer{Name: "tcp/80", Proto: "tcp", Port: 80, Check: "tcp"}}); err == nil {
		t.Error("an offer named like a port")
	}
	// The same offer again changes nothing - not even when it was made.
	must(a.apply(TypeOfferAdd, "web-1", t1, OfferAdd{Node: "web-1", Offer: Offer{Name: "shop", Proto: "tcp", Port: 8080, Check: "tcp"}}))
	a.f.Read(func(s *State) {
		o, ok := s.OfferNamed("web-1", "shop")
		if !ok || !o.Since.Equal(t0) || len(s.Offers["web-1"].Offers) != 2 || s.Offers["web-1"].Offers[0].Name != "dns" {
			t.Errorf("the offers: %+v", s.Offers["web-1"])
		}
	})
	must(a.apply(TypePoolAdd, "lb-1", t0, Pool{Name: "p", First: "10.10.1.20", Bits: 24, Count: 10}))
	must(a.apply(TypeServiceAdd, "lb-1", t0, Service{Name: "web", Pool: "p", VIP: "10.10.1.20", Proto: "tcp", Port: 80}))
	must(a.apply(TypeServiceAdd, "lb-1", t0, Service{Name: "resolver", Pool: "p", VIP: "10.10.1.20", Proto: "udp", Port: 53}))
	must(a.apply(TypeAssign, "lb-1", t0, Assign{Assignments: []Assignment{
		{Service: "web", Node: "web-1", Proto: "tcp", Port: 8080, Weight: 1},
		{Service: "resolver", Node: "web-1", Proto: "udp", Port: 53, Weight: 1},
	}}))
	must(a.apply(TypeHealth, "lb-1", t0, Health{Service: "web", Node: "web-1", Healthy: true}))
	// The application moves: its assignment follows it, and waits for its
	// checks again.
	must(a.apply(TypeOfferAdd, "web-1", t1, OfferAdd{Node: "web-1", Offer: Offer{Name: "shop", Proto: "tcp", Port: 8081, Check: "http:/"}}))
	a.f.Read(func(s *State) {
		x := s.Assignments[AssignmentKey("web", "web-1")]
		if x == nil || x.Port != 8081 || x.Healthy {
			t.Errorf("after the move: %+v", x)
		}
	})
	// Withdrawn, its assignment ends; the other stays.
	if err := a.apply(TypeOfferRemove, "web-1", t1, OfferRemove{Node: "web-1", Name: "nothing"}); err == nil {
		t.Error("withdrew an offer never made")
	}
	must(a.apply(TypeOfferRemove, "web-1", t1, OfferRemove{Node: "web-1", Name: "shop"}))
	a.f.Read(func(s *State) {
		if s.Assignments[AssignmentKey("web", "web-1")] != nil || s.Assignments[AssignmentKey("resolver", "web-1")] == nil {
			t.Errorf("after withdrawing shop: %v", s.Assignments)
		}
		if _, ok := s.OfferNamed("web-1", "shop"); ok {
			t.Error("shop is still offered")
		}
	})
	must(a.apply(TypeOfferRemove, "web-1", t1, OfferRemove{Node: "web-1", Name: "dns"}))
	a.f.Read(func(s *State) {
		if len(s.Offers) != 0 || len(s.Assignments) != 0 {
			t.Errorf("after withdrawing both: %v %v", s.Offers, s.Assignments)
		}
	})
}

// TestTheControlVIPsLease: the default until set, set within its bounds, and
// in the snapshot - every director knows how long to wait for a VIP to lapse.
func TestTheControlVIPsLease(t *testing.T) {
	a := created(t)
	a.f.Read(func(s *State) {
		if s.VIPLease() != DefaultLease {
			t.Errorf("default lease %s", s.VIPLease())
		}
	})
	for _, bad := range []int{0, 2, 3601} {
		if err := a.apply(TypeLease, "lb-1", t0, Lease{Seconds: bad}); err == nil {
			t.Errorf("a lease of %ds was set", bad)
		}
	}
	if err := a.apply(TypeLease, "lb-1", t0, Lease{Seconds: 15}); err != nil {
		t.Fatal(err)
	}
	b, err := a.f.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	g := New()
	if err := g.Restore(io.NopCloser(bytes.NewReader(b))); err != nil {
		t.Fatal(err)
	}
	g.Read(func(s *State) {
		if s.VIPLease() != 15*time.Second || s.Applied != a.index {
			t.Errorf("restored lease %s, applied %d; want 15s, %d", s.VIPLease(), s.Applied, a.index)
		}
	})
}
