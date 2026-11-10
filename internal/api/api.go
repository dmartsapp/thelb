// Package api is what members say to the leader, over mutual TLS (ALPN
// thelb-api/1): directors check in, forward admin changes and renew their
// certificates; backends send their heartbeat - every 2 s, on one connection -
// and are told what to serve, and offer their applications or withdraw them.
// One request and one answer per line.
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
	OpOffer   = "offer"   // a backend offers an application, or withdraws one
	OpLeave   = "leave"   // a director stopping: its member identity out of Raft
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
	Offer   *Offer  `json:"offer,omitempty"`
	Brief   bool    `json:"brief,omitempty"` // live: without IPVS's counters
}

// Offer is a backend's offer of one of its applications, or its withdrawal:
// the leader checks that an application offered answers before it records it.
type Offer struct {
	Op    string        `json:"op"` // add or remove
	Offer cluster.Offer `json:"offer"`
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
	Own      []Own     `json:"own,omitempty"`
	Orders   *Orders   `json:"orders,omitempty"`
}

// Own is a backend's own check of one of its offers, as its last heartbeat
// said: ok or fail.
type Own struct {
	Node  string `json:"node"`
	Offer string `json:"offer"` // proto/port
	Check string `json:"check"`
}

// Change is an admin change: `thelb admin ...` on a director, done by the
// leader.
type Change struct {
	Op        string         `json:"op"` // approve refuse remove pool.add pool.remove service.add service.remove assign unassign lease
	Names     []string       `json:"names,omitempty"`
	VIP       string         `json:"vip,omitempty"` // pool.add: the first address and prefix; service.add: an address
	Count     int            `json:"count,omitempty"`
	Seconds   int            `json:"seconds,omitempty"` // lease
	Interface string         `json:"interface,omitempty"`
	Default   bool           `json:"default,omitempty"`
	Pool      string         `json:"pool,omitempty"`
	Proto     string         `json:"proto,omitempty"`
	Port      int            `json:"port,omitempty"`
	Scheduler string         `json:"scheduler,omitempty"`
	Method    string         `json:"method,omitempty"`
	Offers    []string       `json:"offers,omitempty"` // assign: node:offer, or node:proto/port
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
	// Gone: the leader found this director gone, and took it out of the vote.
	Gone bool `json:"gone,omitempty"`
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

// Beat is a backend's heartbeat: how its own checks of its offers go, and the
// VIPs it holds.
type Beat struct {
	Member  string          `json:"member"`
	Version string          `json:"version"`
	Checks  map[string]bool `json:"checks"` // offer key (proto/port) -> its own check passes
	Holding []string        `json:"holding"`
	Leaving bool            `json:"leaving,omitempty"` // stopping: send it nothing new
	NoNFT   bool            `json:"no_nft,omitempty"`  // its kernel has no nftables: no port mapping, no tunnelled service
}

// Orders are what a backend is to serve - and the offers the cluster holds
// for it, which a backend that started again takes up.
type Orders struct {
	Removed     bool            `json:"removed,omitempty"` // removed from the cluster: stop
	Assignments []Assignment    `json:"assignments"`
	Offers      []cluster.Offer `json:"offers"`
}

// Assignment is one service a backend serves: a backend learns the service's
// name, VIP and port, and serves it on its offer's port - mapping one to the
// other when they differ.
type Assignment struct {
	Service string `json:"service"`
	VIP     string `json:"vip"`
	Proto   string `json:"proto"`
	Port    int    `json:"port"`             // the service's
	AppPort int    `json:"app_port"`         // the application's: the offer's
	Tunnel  bool   `json:"tunnel,omitempty"` // sent by IP-in-IP: the backend unwraps it
}
