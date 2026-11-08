// Package cluster is what Raft carries: the cluster's state, the entries that
// change it, and the state machine that applies them - the same way on every
// director, so every director arrives at the same state.
//
// Entries are written only by the leader, after it has authorised the request
// behind them, and carry the leader's clock: applying an entry reads nothing
// but the entry and the state. Entries are versioned; an entry this build does
// not understand stops the director, rather than being skipped.
package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// EntryVersion is the version of the entries this build writes and applies.
const EntryVersion = 1

// How long a refusal is remembered: a refused node that asks again in that time
// is told again, and stops.
const RefusalMemory = time.Hour

// The entries.
const (
	TypeCreate  = "cluster.create" // the bootstrap: the cluster, its CA, the first director
	TypeLeader  = "leader"         // a director became the leader, and holds the control VIP
	TypePending = "pending.add"    // a node asks to join, and waits for approval
	TypeDrop    = "pending.drop"   // a pending node stopped asking
	TypeApprove = "node.approve"   // a director approved pending nodes
	TypeRefuse  = "node.refuse"    // a director refused a pending node
	TypeAdmit   = "node.admit"     // an approved node joined, with a new member identity
	TypeRemove  = "node.remove"    // a director removed a node
	TypeCert    = "cert.issue"     // a certificate was issued
)

// Entry is one change to the state.
type Entry struct {
	V    int             `json:"v"`
	Type string          `json:"type"`
	At   time.Time       `json:"at"` // the leader's clock
	By   string          `json:"by"` // who asked: a director's name, or "bootstrap"
	Data json.RawMessage `json:"data"`
}

// NewEntry encodes an entry.
func NewEntry(typ, by string, at time.Time, data any) ([]byte, error) {
	d, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Entry{V: EntryVersion, Type: typ, At: at.UTC(), By: by, Data: d})
}

// Info is the cluster itself.
type Info struct {
	Name    string    `json:"name"`
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	CACert  []byte    `json:"ca_cert"`
	CAKey   []byte    `json:"ca_key"` // replicated to directors only, never shown
}

// Leader is the director that leads, since when, in which term.
type Leader struct {
	Name      string    `json:"name"`
	RaftID    string    `json:"raft_id"`
	Term      uint64    `json:"term"`
	Since     time.Time `json:"since"`
	Interface string    `json:"interface"` // where it holds the control VIP
}

// Claim is what a node says about itself when it asks to join. Only the address
// and the MAC address are checked on the wire.
type Claim struct {
	Name        string `json:"name"`
	Role        string `json:"role"`
	Address     string `json:"address"`
	MAC         string `json:"mac"`
	Fingerprint string `json:"fingerprint"`
	Hostname    string `json:"hostname"`
	Version     string `json:"version"`
}

// Same is whether two claims are the same node: what approval is bound to.
func (c Claim) Same(o Claim) bool {
	return c.Name == o.Name && c.Role == o.Role && c.Address == o.Address && c.MAC == o.MAC && c.Fingerprint == o.Fingerprint
}

// Node is an approved node.
type Node struct {
	Claim
	ApprovedBy string    `json:"approved_by"`
	ApprovedAt time.Time `json:"approved_at"`
	// Directors only: the current member identity, its address, and the log
	// index at which it was admitted - a member that has applied that far has
	// caught up.
	RaftID     string    `json:"raft_id,omitempty"`
	RaftAddr   string    `json:"raft_addr,omitempty"`
	AdmitIndex uint64    `json:"admit_index,omitempty"`
	Admitted   time.Time `json:"admitted"`
}

// Pending is a node waiting for approval.
type Pending struct {
	Claim
	Since time.Time `json:"since"`
}

// Refusal is a node a director refused.
type Refusal struct {
	Claim
	By string    `json:"by"`
	At time.Time `json:"at"`
}

// Cert is a certificate the cluster issued - never its key.
type Cert struct {
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Serial      string    `json:"serial"`
	Fingerprint string    `json:"fingerprint"`
	NotAfter    time.Time `json:"not_after"`
}

// The entries' data.
type (
	// Create is TypeCreate's.
	Create struct {
		Info Info `json:"info"`
		Node Node `json:"node"`
		Cert Cert `json:"cert"`
	}
	// Names is TypeApprove's.
	Names struct {
		Names []string `json:"names"`
	}
	// Name is TypeDrop's, TypeRefuse's and TypeRemove's.
	Name struct {
		Name string `json:"name"`
	}
	// Admit is TypeAdmit's.
	Admit struct {
		Claim    Claim  `json:"claim"`
		RaftID   string `json:"raft_id"`
		RaftAddr string `json:"raft_addr"`
		Cert     Cert   `json:"cert"`
	}
)

// State is the cluster's state.
type State struct {
	Cluster *Info                `json:"cluster"`
	Leader  *Leader              `json:"leader"`
	Nodes   map[string]*Node     `json:"nodes"`   // approved, by name
	Pending map[string]*Pending  `json:"pending"` // by name
	Refused map[string]*Refusal  `json:"refused"` // by name, for RefusalMemory
	Certs   map[string]*Cert     `json:"certs"`   // the latest of each node, by name
	Revoked map[string]time.Time `json:"revoked"` // serial -> when it would have expired
}

func newState() *State {
	return &State{
		Nodes:   map[string]*Node{},
		Pending: map[string]*Pending{},
		Refused: map[string]*Refusal{},
		Certs:   map[string]*Cert{},
		Revoked: map[string]time.Time{},
	}
}

// NodeByRaftID is the node whose current member identity is id.
func (s *State) NodeByRaftID(id string) *Node {
	for _, n := range s.Nodes {
		if n.RaftID == id {
			return n
		}
	}
	return nil
}

// Sorted is a map's keys, sorted.
func Sorted[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// ErrUnknownEntry is an entry this build cannot apply: the director stops.
var ErrUnknownEntry = errors.New("an entry this version of theLB does not understand")

// FSM is the state machine Raft drives.
type FSM struct {
	mu      sync.RWMutex
	s       *State
	changed chan struct{}
	// OnFatal is called, once, with an entry this build cannot apply.
	OnFatal func(error)
	fatal   sync.Once
}

// New is an empty state machine: a director that has not yet joined.
func New() *FSM {
	return &FSM{s: newState(), changed: make(chan struct{})}
}

// Read calls fn with the state, which fn must not change or keep.
func (f *FSM) Read(fn func(*State)) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	fn(f.s)
}

// Changed is closed at the next change to the state.
func (f *FSM) Changed() <-chan struct{} {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.changed
}

// Apply applies a committed entry. It returns nil, or an error saying why the
// entry changed nothing.
func (f *FSM) Apply(l *raft.Log) any {
	if l.Type != raft.LogCommand {
		return nil
	}
	var e Entry
	if err := json.Unmarshal(l.Data, &e); err != nil {
		return f.stop(fmt.Errorf("%w: unreadable entry at index %d", ErrUnknownEntry, l.Index))
	}
	if e.V != EntryVersion {
		return f.stop(fmt.Errorf("%w: version %d of %q at index %d", ErrUnknownEntry, e.V, e.Type, l.Index))
	}
	err := f.applyLocked(e, l.Index)
	if errors.Is(err, ErrUnknownEntry) {
		return f.stop(err)
	}
	return err
}

func (f *FSM) applyLocked(e Entry, index uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.apply(f.s, e, index)
	if errors.Is(err, ErrUnknownEntry) {
		return err
	}
	// Forget what has expired, by the entry's clock: the same on every director.
	for name, r := range f.s.Refused {
		if e.At.Sub(r.At) > RefusalMemory {
			delete(f.s.Refused, name)
		}
	}
	for serial, until := range f.s.Revoked {
		if e.At.After(until) {
			delete(f.s.Revoked, serial)
		}
	}
	close(f.changed)
	f.changed = make(chan struct{})
	return err
}

func (f *FSM) stop(err error) error {
	f.fatal.Do(func() {
		if f.OnFatal != nil {
			f.OnFatal(err)
		}
	})
	return err
}

func (f *FSM) apply(s *State, e Entry, index uint64) error {
	switch e.Type {
	case TypeCreate:
		var d Create
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		if s.Cluster != nil {
			return errors.New("the cluster exists already")
		}
		info := d.Info
		s.Cluster = &info
		n := d.Node
		n.AdmitIndex = index
		s.Nodes[n.Name] = &n
		c := d.Cert
		s.Certs[c.Name] = &c
		return nil
	case TypeLeader:
		var d Leader
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		if s.Leader != nil && d.Term < s.Leader.Term {
			return fmt.Errorf("term %d is older than the leader's, %d", d.Term, s.Leader.Term)
		}
		s.Leader = &d
		return nil
	case TypePending:
		var d Pending
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		if old, ok := s.Pending[d.Name]; ok && old.Same(d.Claim) {
			d.Since = old.Since
		}
		delete(s.Refused, d.Name)
		s.Pending[d.Name] = &d
		return nil
	case TypeDrop:
		var d Name
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		delete(s.Pending, d.Name)
		return nil
	case TypeApprove:
		var d Names
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		var missing []string
		for _, name := range d.Names {
			p, ok := s.Pending[name]
			if !ok {
				missing = append(missing, name)
				continue
			}
			delete(s.Pending, name)
			n := &Node{Claim: p.Claim, ApprovedBy: e.By, ApprovedAt: e.At}
			if old, ok := s.Nodes[name]; ok {
				// Approved again with a new address or machine: the member
				// identity is the old one's until the node is admitted.
				n.RaftID, n.RaftAddr, n.AdmitIndex, n.Admitted = old.RaftID, old.RaftAddr, old.AdmitIndex, old.Admitted
			}
			s.Nodes[name] = n
		}
		if len(missing) > 0 {
			return fmt.Errorf("not pending: %v", missing)
		}
		return nil
	case TypeRefuse:
		var d Name
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		p, ok := s.Pending[d.Name]
		if !ok {
			return fmt.Errorf("%s is not pending", d.Name)
		}
		delete(s.Pending, d.Name)
		s.Refused[d.Name] = &Refusal{Claim: p.Claim, By: e.By, At: e.At}
		return nil
	case TypeAdmit:
		var d Admit
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		n, ok := s.Nodes[d.Claim.Name]
		if !ok || !n.Same(d.Claim) {
			return fmt.Errorf("%s is not approved as it claims to be", d.Claim.Name)
		}
		n.Claim = d.Claim // the same node; its version and hostname may be new
		n.RaftID, n.RaftAddr, n.AdmitIndex, n.Admitted = d.RaftID, d.RaftAddr, index, e.At
		if old, ok := s.Certs[d.Cert.Name]; ok && old.Serial != d.Cert.Serial {
			s.Revoked[old.Serial] = old.NotAfter
		}
		c := d.Cert
		s.Certs[c.Name] = &c
		return nil
	case TypeRemove:
		var d Name
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		if _, ok := s.Nodes[d.Name]; !ok {
			return fmt.Errorf("%s is not a node of this cluster", d.Name)
		}
		delete(s.Nodes, d.Name)
		if c, ok := s.Certs[d.Name]; ok {
			s.Revoked[c.Serial] = c.NotAfter
			delete(s.Certs, d.Name)
		}
		return nil
	case TypeCert:
		var d Cert
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		if _, ok := s.Nodes[d.Name]; !ok {
			return fmt.Errorf("%s is not a node of this cluster", d.Name)
		}
		s.Certs[d.Name] = &d
		return nil
	default:
		return fmt.Errorf("%w: %q at index %d", ErrUnknownEntry, e.Type, index)
	}
}

// Snapshot is the whole state, for a member that is far behind - every new
// member identity starts from one.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	b, err := json.Marshal(f.s)
	if err != nil {
		return nil, err
	}
	return snapshot(b), nil
}

// Restore replaces the state with a snapshot's.
func (f *FSM) Restore(r io.ReadCloser) error {
	defer func() { _ = r.Close() }()
	s := newState()
	if err := json.NewDecoder(r).Decode(s); err != nil {
		return fmt.Errorf("reading a snapshot: %w", err)
	}
	fresh := newState()
	if s.Nodes == nil {
		s.Nodes = fresh.Nodes
	}
	if s.Pending == nil {
		s.Pending = fresh.Pending
	}
	if s.Refused == nil {
		s.Refused = fresh.Refused
	}
	if s.Certs == nil {
		s.Certs = fresh.Certs
	}
	if s.Revoked == nil {
		s.Revoked = fresh.Revoked
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.s = s
	close(f.changed)
	f.changed = make(chan struct{})
	return nil
}

type snapshot []byte

func (s snapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}

func (snapshot) Release() {}
