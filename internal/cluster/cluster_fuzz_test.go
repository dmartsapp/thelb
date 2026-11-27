package cluster

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/hashicorp/raft"
)

var entryTypes = []string{TypeCreate, TypeLeader, TypePending, TypeDrop, TypeApprove, TypeRefuse, TypeAdmit, TypeRemove, TypeCert,
	TypePoolAdd, TypePoolRemove, TypeServiceAdd, TypeServiceRemove, TypeOffers, TypeOfferAdd, TypeOfferRemove, TypeAssign, TypeUnassign,
	TypeHealth, TypeLease, TypeKubeAlloc, TypeKubeRelease, "no.such.type"}

// FuzzApply: entries come only from the leader, but a bug there, or a newer
// leader, must stop a director with an error - never crash it. Every kind of
// entry, with any data, on a created cluster; then the state still writes
// itself as a snapshot that reads back the same.
func FuzzApply(f *testing.F) {
	f.Add(uint8(2), []byte(`{"claim":{"name":"lb-2","role":"director"},"since":"2026-11-09T10:00:01Z"}`), uint8(0), []byte(`{}`))
	f.Add(uint8(9), []byte(`{"pool":"main","vip":"10.0.0.50/24","count":4,"default":true}`), uint8(11), []byte(`{"names":["web"],"proto":"tcp","port":80}`))
	f.Add(uint8(16), []byte(`{"names":["web"],"offers":["b1:web"],"weights":{"b1":-1}}`), uint8(19), []byte(`{"seconds":0}`))
	f.Add(uint8(20), []byte(`{"key":"default/web","vips":[{"vip":"not an address"}]}`), uint8(22), []byte(`null`))
	f.Add(uint8(4), []byte(`{"names":[null,"",""]}`), uint8(7), []byte(`[]`))
	f.Fuzz(func(t *testing.T, t1 uint8, d1 []byte, t2 uint8, d2 []byte) {
		a := created(t)
		for _, e := range []struct {
			typ  string
			data []byte
		}{{entryTypes[int(t1)%len(entryTypes)], d1}, {entryTypes[int(t2)%len(entryTypes)], d2}} {
			b, err := json.Marshal(Entry{V: EntryVersion, Type: e.typ, At: t0, By: "lb-1", Data: json.RawMessage(validJSON(e.data))})
			if err != nil {
				t.Fatal(err)
			}
			a.index++
			_ = a.f.Apply(&raft.Log{Index: a.index, Type: raft.LogCommand, Data: b})
		}
		snap, err := a.f.Bytes()
		if err != nil {
			t.Fatalf("the state does not write itself: %v", err)
		}
		g := New()
		if err := g.Restore(io.NopCloser(bytes.NewReader(snap))); err != nil {
			t.Fatalf("the snapshot does not read back: %v", err)
		}
		again, _ := g.Bytes()
		if !bytes.Equal(snap, again) {
			t.Fatalf("the snapshot read back differs")
		}
	})
}

func validJSON(b []byte) []byte {
	if json.Valid(b) {
		return b
	}
	return []byte(`{}`)
}

// FuzzApplyRaw: an entry's bytes as they are - not JSON, another version,
// anything. Apply returns, and stops the director when it cannot read it.
func FuzzApplyRaw(f *testing.F) {
	f.Add([]byte(`{"v":1,"type":"lease","data":{"seconds":6}}`))
	f.Add([]byte(`{"v":99,"type":"lease"}`))
	f.Add([]byte(`not json`))
	f.Fuzz(func(t *testing.T, b []byte) {
		a := created(t)
		stopped := false
		a.f.OnFatal = func(error) { stopped = true }
		r := a.f.Apply(&raft.Log{Index: 2, Type: raft.LogCommand, Data: b})
		if !json.Valid(b) && !stopped {
			t.Fatalf("an unreadable entry did not stop the director: %v", r)
		}
	})
}

// FuzzRestore: a snapshot from another director. Restore reads it or says
// why; it never crashes.
func FuzzRestore(f *testing.F) {
	a := created(&testing.T{})
	snap, _ := a.f.Bytes()
	f.Add(snap)
	f.Add([]byte(`{"nodes":{"x":null},"services":{"":{}}}`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = New().Restore(io.NopCloser(bytes.NewReader(b)))
	})
}
