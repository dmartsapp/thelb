// Package api is what members say to the leader, over mutual TLS (ALPN
// thelb-api/1): directors check in, forward admin changes and renew their
// certificates; backends send their heartbeat - every 2 s, on one connection -
// and are told what to serve. One request and one answer per line.
package api

import (
	"time"

	"github.com/dmartsapp/thelb/internal/cluster"
)

// The operations.
const (
	OpCheckIn = "checkin" // a director's, streamed
	OpLive    = "live"    // what only the leader knows: check-ins, checks, counters
	OpChange  = "change"  // an admin change, from a director
	OpRenew   = "renew"   // a certificate, renewed
	OpBackend = "backend" // a backend's heartbeat, streamed
)

// Request is a member's.
type Request struct {
	V       int     `json:"v"`
	Op      string  `json:"op"`
	CSR     []byte  `json:"csr,omitempty"`
	RaftID  string  `json:"raft_id,omitempty"`
	Applied uint64  `json:"applied,omitempty"`
	Version string  `json:"version,omitempty"`
	Change  *Change `json:"change,omitempty"`
	Beat    *Beat   `json:"beat,omitempty"`
}

// Response is the leader's.
type Response struct {
	V        int       `json:"v"`
	OK       bool      `json:"ok"`
	Error    string    `json:"error,omitempty"`
	Result   *Result   `json:"result,omitempty"`
	Cert     []byte    `json:"cert,omitempty"`
	CheckIns []CheckIn `json:"check_ins,omitempty"`
	Live     []Live    `json:"live,omitempty"`
	Orders   *Orders   `json:"orders,omitempty"`
}

// Change is an admin change: `thelb admin ...` on a director, done by the
// leader.
type Change struct {
	Op        string         `json:"op"` // approve refuse remove pool.add pool.remove service.add service.remove assign unassign
	Names     []string       `json:"names,omitempty"`
	VIP       string         `json:"vip,omitempty"` // pool.add: the first address and prefix; service.add: an address
	Count     int            `json:"count,omitempty"`
	Interface string         `json:"interface,omitempty"`
	Default   bool           `json:"default,omitempty"`
	Pool      string         `json:"pool,omitempty"`
	Proto     string         `json:"proto,omitempty"`
	Port      int            `json:"port,omitempty"`
	Scheduler string         `json:"scheduler,omitempty"`
	Offers    []string       `json:"offers,omitempty"` // assign: node:proto/port
	Weights   map[string]int `json:"weights,omitempty"`
}

// Result is what a change did.
type Result struct {
	Names   []string `json:"names,omitempty"`
	Message string   `json:"message,omitempty"`
}

// CheckIn is a member's last check-in with the leader - a director's, or a
// backend's heartbeat.
type CheckIn struct {
	Name    string    `json:"name"`
	RaftID  string    `json:"raft_id"`
	At      time.Time `json:"at"`
	Applied uint64    `json:"applied"`
	Version string    `json:"version"`
}

// Live is an assignment as the leader sees it now.
type Live struct {
	Service  string `json:"service"`
	Node     string `json:"node"`
	Own      string `json:"own"`      // the backend's own check: ok, fail, or "-" without a heartbeat
	Director string `json:"director"` // the leader's check
	Weight   int    `json:"weight"`   // what IPVS has
	Conns    uint64 `json:"conns"`
	Packets  uint64 `json:"packets"`
	Bytes    uint64 `json:"bytes"`
}

// Beat is a backend's heartbeat: what it offers, how its own checks go, and the
// VIPs it holds.
type Beat struct {
	Member  string          `json:"member"`
	Version string          `json:"version"`
	Offers  []cluster.Offer `json:"offers"`
	Checks  map[string]bool `json:"checks"` // offer key -> its own check passes
	Holding []string        `json:"holding"`
	Leaving bool            `json:"leaving,omitempty"` // stopping: send it nothing new
}

// Orders are what a backend is to serve.
type Orders struct {
	Removed     bool         `json:"removed,omitempty"` // removed from the cluster: stop
	Assignments []Assignment `json:"assignments"`
}

// Assignment is one service a backend serves: a backend learns the service's
// name and VIP, and serves it on its offer's port.
type Assignment struct {
	Service string `json:"service"`
	VIP     string `json:"vip"`
	Proto   string `json:"proto"`
	Port    int    `json:"port"`
}
