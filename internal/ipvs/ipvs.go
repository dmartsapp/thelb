// Package ipvs is the little of IPVS theLB needs: virtual services and their
// destinations, programmed over IPVS's generic netlink interface - no ipvsadm -
// with each destination's counters.
package ipvs

import (
	"fmt"
	"net/netip"
	"strings"
)

// Protocols.
const (
	TCP  = 6
	UDP  = 17
	SCTP = 132
)

// ProtoNumber is a protocol's number, from its name.
func ProtoNumber(name string) (uint16, error) {
	switch strings.ToLower(name) {
	case "tcp":
		return TCP, nil
	case "udp":
		return UDP, nil
	case "sctp":
		return SCTP, nil
	}
	return 0, fmt.Errorf("%q is not tcp, udp or sctp", name)
}

// ProtoName is a protocol's name.
func ProtoName(n uint16) string {
	switch n {
	case TCP:
		return "tcp"
	case UDP:
		return "udp"
	case SCTP:
		return "sctp"
	}
	return fmt.Sprint(n)
}

// Service is a virtual service: an address, a protocol and a port.
type Service struct {
	Addr      netip.Addr
	Protocol  uint16
	Port      uint16
	Scheduler string
}

// Key is what identifies a service.
func (s Service) Key() string {
	return fmt.Sprintf("%s/%s", ProtoName(s.Protocol), netip.AddrPortFrom(s.Addr, s.Port))
}

// Dest is a destination - a backend - of a service, by direct routing.
type Dest struct {
	Addr   netip.Addr
	Port   uint16
	Weight uint32
	Tunnel bool // IP-in-IP to the backend, not direct routing
	Stats  Stats
}

// Stats are what IPVS counted for a destination: the connections it scheduled
// there, and the packets and bytes it forwarded. Direct routing never sees the
// replies, so there is no outbound count.
type Stats struct {
	Conns    uint64
	Packets  uint64
	Bytes    uint64
	Active   uint32 // connections established now
	Inactive uint32 // the others it still tracks: closing, or not yet established
}

// Daemon is one of IPVS's connection-synchronisation daemons, kernel threads in a
// network namespace: a master sends that namespace's connections, by multicast on
// Interface (224.0.0.81, UDP port 8848, TTL 1), and a backup takes in those of
// SyncID into its own connection table - so a director that takes the VIPs over
// already knows where every connection in progress goes.
type Daemon struct {
	Master    bool
	Interface string
	SyncID    uint32
}

// Role is "master" or "backup".
func (d Daemon) Role() string {
	if d.Master {
		return "master"
	}
	return "backup"
}
