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
	Conns   uint64
	Packets uint64
	Bytes   uint64
}
