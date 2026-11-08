// Package nft is theLB's own nftables table, inet thelb, over netlink - no nft
// command: on a backend, a destination NAT rule for each service whose port
// differs from its application's. Packets to VIP:80 go to port 8080; the
// kernel's connection tracking turns the replies back into VIP:80, for TCP,
// UDP and SCTP alike, ICMP errors and fragments included.
package nft

import (
	"fmt"
	"net/netip"
	"strings"
)

// Table is theLB's table, in the inet family.
const Table = "thelb"

// Chain is its one chain: destination NAT, at prerouting.
const Chain = "portmap"

// Map is one mapping: packets to VIP, Proto, Port go to AppPort.
type Map struct {
	VIP     netip.Addr
	Proto   string
	Port    uint16
	AppPort uint16
}

func (m Map) String() string {
	return fmt.Sprintf("%s %s/%d -> %d", m.VIP, m.Proto, m.Port, m.AppPort)
}

func protoNumber(p string) (byte, error) {
	switch strings.ToLower(p) {
	case "tcp":
		return 6, nil
	case "udp":
		return 17, nil
	case "sctp":
		return 132, nil
	}
	return 0, fmt.Errorf("%q is not tcp, udp or sctp", p)
}
