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
	"net/netip"
	"regexp"
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

	TypePoolAdd       = "pool.add"       // a director registered a pool of service VIPs
	TypePoolRemove    = "pool.remove"    // a director removed one
	TypeServiceAdd    = "service.add"    // a director created a service
	TypeServiceRemove = "service.remove" // a director removed one
	TypeOffers        = "offers"         // a backend's whole list of offers - written by older leaders only
	TypeOfferAdd      = "offer.add"      // a backend offers an application, the leader having checked it answers
	TypeOfferRemove   = "offer.remove"   // a backend withdrew one
	TypeAssign        = "assign"         // a director assigned offers to a service
	TypeUnassign      = "unassign"       // a director took one back
	TypeHealth        = "health"         // an assignment became ready to serve, or stopped being
	TypeLease         = "lease"          // a director set how long the kernel keeps the control VIP without its leader
)

// The control VIP's lease: the leader adds the VIP with this lifetime, and
// renews it every third of it, so the kernel removes the VIP of a leader that
// was killed or hangs by itself; a director taking over waits it out. In the
// replicated state, so every director knows how long to wait.
const (
	DefaultLease = 6 * time.Second
	MinLease     = 3 * time.Second
	MaxLease     = time.Hour
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
	// Lease is the control VIP's lease, in seconds; 0: DefaultLease.
	Lease int `json:"lease,omitempty"`
}

// VIPLease is the control VIP's lease.
func (s *State) VIPLease() time.Duration {
	if s.Cluster == nil || s.Cluster.Lease == 0 {
		return DefaultLease
	}
	return time.Duration(s.Cluster.Lease) * time.Second
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
	// SetOffers is TypeOffers's.
	SetOffers struct {
		Node   string  `json:"node"`
		Offers []Offer `json:"offers"`
	}
	// OfferAdd is TypeOfferAdd's.
	OfferAdd struct {
		Node  string `json:"node"`
		Offer Offer  `json:"offer"`
	}
	// OfferRemove is TypeOfferRemove's.
	OfferRemove struct {
		Node string `json:"node"`
		Name string `json:"name"`
	}
	// Assign is TypeAssign's.
	Assign struct {
		Assignments []Assignment `json:"assignments"`
	}
	// Unassign is TypeUnassign's.
	Unassign struct {
		Service string `json:"service"`
		Node    string `json:"node"`
	}
	// Health is TypeHealth's.
	Health struct {
		Service string `json:"service"`
		Node    string `json:"node"`
		Healthy bool   `json:"healthy"`
	}
	// Lease is TypeLease's: in seconds.
	Lease struct {
		Seconds int `json:"seconds"`
	}
	// Admit is TypeAdmit's.
	Admit struct {
		Claim    Claim  `json:"claim"`
		RaftID   string `json:"raft_id"`
		RaftAddr string `json:"raft_addr"`
		Cert     Cert   `json:"cert"`
	}
)

// Pool is a range of service VIPs: Count addresses from First, in the network
// First/Bits.
type Pool struct {
	Name      string    `json:"name"`
	First     string    `json:"first"`
	Bits      int       `json:"bits"`
	Count     int       `json:"count"`
	Interface string    `json:"interface,omitempty"` // "": the one in its network
	Default   bool      `json:"default,omitempty"`
	By        string    `json:"by"`
	At        time.Time `json:"at"`
}

// Contains is whether a is one of the pool's addresses.
func (p *Pool) Contains(a netip.Addr) bool {
	first, err := netip.ParseAddr(p.First)
	if err != nil || a.BitLen() != first.BitLen() {
		return false
	}
	for i, x := 0, first; i < p.Count; i, x = i+1, x.Next() {
		if x == a {
			return true
		}
	}
	return false
}

// Service is a service: a VIP, a protocol and a port, with its scheduler.
type Service struct {
	Name      string    `json:"name"`
	Pool      string    `json:"pool"`
	VIP       string    `json:"vip"`
	Proto     string    `json:"proto"`
	Port      int       `json:"port"`
	Scheduler string    `json:"scheduler"`
	Method    string    `json:"method,omitempty"` // "direct" (the default) or "tunnel"
	By        string    `json:"by"`
	At        time.Time `json:"at"`
}

// Tunnel is whether the service reaches its backends by IP-in-IP.
func (s *Service) Tunnel() bool { return s.Method == "tunnel" }

// Offer is what a backend serves: an application, under a name the backend
// gives it, at a protocol and port, with how to check it - "tcp", "none" or
// "http:<path>". A backend's offers differ in name and in protocol and port.
type Offer struct {
	Name  string    `json:"name"`
	Proto string    `json:"proto"`
	Port  int       `json:"port"`
	Check string    `json:"check"`
	Since time.Time `json:"since"` // when the cluster recorded it
}

// Key is an offer's protocol and port: tcp/8080.
func (o Offer) Key() string { return fmt.Sprintf("%s/%d", o.Proto, o.Port) }

// Same is whether two offers are the same, but for when they were recorded.
func (o Offer) Same(x Offer) bool {
	return o.Name == x.Name && o.Proto == x.Proto && o.Port == x.Port && o.Check == x.Check
}

// DefaultOfferName is the name of an offer given none: tcp-8080.
func DefaultOfferName(proto string, port int) string { return fmt.Sprintf("%s-%d", proto, port) }

var offerNameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)

// OfferNameOK is whether name may name an offer: letters, digits, dots,
// hyphens and underscores, up to 63, beginning and ending with a letter or a
// digit - never a "/" or a ":", which say a port.
func OfferNameOK(name string) bool { return offerNameRE.MatchString(name) }

// Offers are a backend's offers, and since when it has made them.
type Offers struct {
	Offers []Offer   `json:"offers"`
	Since  time.Time `json:"since"`
}

// Assignment is a backend's offer serving a service.
type Assignment struct {
	Service string    `json:"service"`
	Node    string    `json:"node"`
	Proto   string    `json:"proto"`
	Port    int       `json:"port"` // the application's
	Weight  int       `json:"weight"`
	Healthy bool      `json:"healthy"` // both checks pass, the backend holds the VIP and reports
	By      string    `json:"by"`
	At      time.Time `json:"at"`
}

// AssignmentKey is an assignment's key in the state.
func AssignmentKey(service, node string) string { return service + "/" + node }

// OfferNamed is the backend's offer of that name.
func (s *State) OfferNamed(node, name string) (Offer, bool) {
	if o := s.Offers[node]; o != nil {
		for _, x := range o.Offers {
			if x.Name == name {
				return x, true
			}
		}
	}
	return Offer{}, false
}

// OfferAt is the backend's offer at that protocol and port.
func (s *State) OfferAt(node, proto string, port int) (Offer, bool) {
	if o := s.Offers[node]; o != nil {
		for _, x := range o.Offers {
			if x.Proto == proto && x.Port == port {
				return x, true
			}
		}
	}
	return Offer{}, false
}

// endAssignments ends the node's assignments of the offer at proto and port.
func (s *State) endAssignments(node, proto string, port int) {
	for k, a := range s.Assignments {
		if a.Node == node && a.Proto == proto && a.Port == port {
			delete(s.Assignments, k)
		}
	}
}

// State is the cluster's state.
type State struct {
	Cluster     *Info                  `json:"cluster"`
	Leader      *Leader                `json:"leader"`
	Nodes       map[string]*Node       `json:"nodes"`   // approved, by name
	Pending     map[string]*Pending    `json:"pending"` // by name
	Refused     map[string]*Refusal    `json:"refused"` // by name, for RefusalMemory
	Certs       map[string]*Cert       `json:"certs"`   // the latest of each node, by name
	Revoked     map[string]time.Time   `json:"revoked"` // serial -> when it would have expired
	Pools       map[string]*Pool       `json:"pools"`
	Services    map[string]*Service    `json:"services"`
	Offers      map[string]*Offers     `json:"offers"`      // by backend
	Assignments map[string]*Assignment `json:"assignments"` // by AssignmentKey
	// Applied is the index of the last entry applied: a director that stands
	// alone replays its log past it.
	Applied uint64 `json:"applied,omitempty"`
}

func newState() *State {
	return &State{
		Nodes:       map[string]*Node{},
		Pending:     map[string]*Pending{},
		Refused:     map[string]*Refusal{},
		Certs:       map[string]*Cert{},
		Revoked:     map[string]time.Time{},
		Pools:       map[string]*Pool{},
		Services:    map[string]*Service{},
		Offers:      map[string]*Offers{},
		Assignments: map[string]*Assignment{},
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
	f.s.Applied = index
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
		delete(s.Offers, d.Name)
		for k, a := range s.Assignments {
			if a.Node == d.Name {
				delete(s.Assignments, k)
			}
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
	case TypePoolAdd:
		var d Pool
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		if _, ok := s.Pools[d.Name]; ok {
			return fmt.Errorf("a pool named %s exists", d.Name)
		}
		d.By, d.At = e.By, e.At
		s.Pools[d.Name] = &d
		return nil
	case TypePoolRemove:
		var d Name
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		if _, ok := s.Pools[d.Name]; !ok {
			return fmt.Errorf("no pool named %s", d.Name)
		}
		for _, v := range s.Services {
			if v.Pool == d.Name {
				return fmt.Errorf("the pool %s holds the VIP of the service %s", d.Name, v.Name)
			}
		}
		delete(s.Pools, d.Name)
		return nil
	case TypeServiceAdd:
		var d Service
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		if _, ok := s.Services[d.Name]; ok {
			return fmt.Errorf("a service named %s exists", d.Name)
		}
		if _, ok := s.Pools[d.Pool]; !ok {
			return fmt.Errorf("no pool named %s", d.Pool)
		}
		for _, v := range s.Services {
			if v.VIP == d.VIP && v.Proto == d.Proto && v.Port == d.Port {
				return fmt.Errorf("%s %s/%d is the service %s's", d.VIP, d.Proto, d.Port, v.Name)
			}
		}
		d.By, d.At = e.By, e.At
		s.Services[d.Name] = &d
		return nil
	case TypeServiceRemove:
		var d Name
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		if _, ok := s.Services[d.Name]; !ok {
			return fmt.Errorf("no service named %s", d.Name)
		}
		delete(s.Services, d.Name)
		for k, a := range s.Assignments {
			if a.Service == d.Name {
				delete(s.Assignments, k)
			}
		}
		return nil
	case TypeOffers:
		var d SetOffers
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		if n := s.Nodes[d.Node]; n == nil || n.Role != "backend" {
			return fmt.Errorf("%s is not a backend of this cluster", d.Node)
		}
		for i := range d.Offers {
			if d.Offers[i].Name == "" {
				d.Offers[i].Name = DefaultOfferName(d.Offers[i].Proto, d.Offers[i].Port)
			}
			if d.Offers[i].Since.IsZero() {
				d.Offers[i].Since = e.At
			}
		}
		s.Offers[d.Node] = &Offers{Offers: d.Offers, Since: e.At}
		// An assignment whose offer was withdrawn ends.
		for k, a := range s.Assignments {
			if a.Node != d.Node {
				continue
			}
			kept := false
			for _, o := range d.Offers {
				kept = kept || (o.Proto == a.Proto && o.Port == a.Port)
			}
			if !kept {
				delete(s.Assignments, k)
			}
		}
		return nil
	case TypeOfferAdd:
		var d OfferAdd
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		if n := s.Nodes[d.Node]; n == nil || n.Role != "backend" {
			return fmt.Errorf("%s is not a backend of this cluster", d.Node)
		}
		o := d.Offer
		if !OfferNameOK(o.Name) {
			return fmt.Errorf("%q cannot name an offer", o.Name)
		}
		if x, ok := s.OfferAt(d.Node, o.Proto, o.Port); ok && x.Name != o.Name {
			return fmt.Errorf("%s offers %s already, as %s", d.Node, o.Key(), x.Name)
		}
		list := s.Offers[d.Node]
		if list == nil {
			list = &Offers{}
			s.Offers[d.Node] = list
		}
		o.Since = e.At
		for i, x := range list.Offers {
			if x.Name != o.Name {
				continue
			}
			if x.Same(o) {
				return nil
			}
			list.Since = e.At
			// The same application, moved or checked otherwise: its assignments
			// follow it to its new port - or end, for another protocol - and wait
			// for its checks again.
			for k, a := range s.Assignments {
				if a.Node != d.Node || a.Proto != x.Proto || a.Port != x.Port {
					continue
				}
				if o.Proto != x.Proto {
					delete(s.Assignments, k)
					continue
				}
				a.Port = o.Port
				if o.Port != x.Port {
					a.Healthy = false
				}
			}
			list.Offers[i] = o
			return nil
		}
		list.Since = e.At
		list.Offers = append(list.Offers, o)
		slices.SortFunc(list.Offers, func(a, b Offer) int {
			switch {
			case a.Name < b.Name:
				return -1
			case a.Name > b.Name:
				return 1
			}
			return 0
		})
		return nil
	case TypeOfferRemove:
		var d OfferRemove
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		x, ok := s.OfferNamed(d.Node, d.Name)
		if !ok {
			return fmt.Errorf("%s offers nothing named %s", d.Node, d.Name)
		}
		list := s.Offers[d.Node]
		list.Offers = slices.DeleteFunc(list.Offers, func(o Offer) bool { return o.Name == d.Name })
		list.Since = e.At
		if len(list.Offers) == 0 {
			delete(s.Offers, d.Node)
		}
		// An assignment whose offer was withdrawn ends.
		s.endAssignments(d.Node, x.Proto, x.Port)
		return nil
	case TypeAssign:
		var d Assign
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		for _, a := range d.Assignments {
			if s.Services[a.Service] == nil {
				return fmt.Errorf("no service named %s", a.Service)
			}
			if n := s.Nodes[a.Node]; n == nil || n.Role != "backend" {
				return fmt.Errorf("%s is not a backend of this cluster", a.Node)
			}
		}
		for _, a := range d.Assignments {
			a.By, a.At = e.By, e.At
			k := AssignmentKey(a.Service, a.Node)
			if old, ok := s.Assignments[k]; ok && old.Proto == a.Proto && old.Port == a.Port {
				a.Healthy = old.Healthy // a new weight: still as healthy as it was
			}
			c := a
			s.Assignments[k] = &c
		}
		return nil
	case TypeUnassign:
		var d Unassign
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		k := AssignmentKey(d.Service, d.Node)
		if _, ok := s.Assignments[k]; !ok {
			return fmt.Errorf("%s is not assigned to %s", d.Node, d.Service)
		}
		delete(s.Assignments, k)
		return nil
	case TypeLease:
		var d Lease
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		if s.Cluster == nil {
			return errors.New("there is no cluster yet")
		}
		if l := time.Duration(d.Seconds) * time.Second; l < MinLease || l > MaxLease {
			return fmt.Errorf("the control VIP's lease is %s to %s, not %ds", MinLease, MaxLease, d.Seconds)
		}
		s.Cluster.Lease = d.Seconds
		return nil
	case TypeHealth:
		var d Health
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnknownEntry, e.Type, err)
		}
		a, ok := s.Assignments[AssignmentKey(d.Service, d.Node)]
		if !ok {
			return fmt.Errorf("%s is not assigned to %s", d.Node, d.Service)
		}
		a.Healthy = d.Healthy
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

// Bytes is the state as Snapshot writes it - for a director that stands alone,
// seeding a Raft of its own with it.
func (f *FSM) Bytes() ([]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return json.Marshal(f.s)
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
	if s.Pools == nil {
		s.Pools = fresh.Pools
	}
	if s.Services == nil {
		s.Services = fresh.Services
	}
	if s.Offers == nil {
		s.Offers = fresh.Offers
	}
	if s.Assignments == nil {
		s.Assignments = fresh.Assignments
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
