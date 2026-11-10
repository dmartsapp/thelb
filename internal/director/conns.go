package director

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/dmartsapp/thelb/internal/cluster"
)

// Conn is a connection IPVS forwards: from a client, to a VIP and port, sent
// to a backend.
type Conn struct {
	Proto  string // TCP, UDP, SCTP
	Client netip.AddrPort
	VIP    netip.AddrPort
	Dest   netip.AddrPort
	State  string
}

// ParseConns reads IPVS's connection table, as /proc/net/ip_vs_conn writes it:
// a header, then a connection a line - IPv4 addresses and every port in
// hexadecimal, IPv6 addresses written out.
func ParseConns(table string) []Conn {
	var out []Conn
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		var c Conn
		ok := true
		ap := func(addr, port string) netip.AddrPort {
			p, err := strconv.ParseUint(port, 16, 16)
			if err != nil {
				ok = false
				return netip.AddrPort{}
			}
			if len(addr) == 8 && !strings.Contains(addr, ":") {
				v, err := strconv.ParseUint(addr, 16, 32)
				if err != nil {
					ok = false
					return netip.AddrPort{}
				}
				return netip.AddrPortFrom(netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), uint16(p))
			}
			a, err := netip.ParseAddr(addr)
			if err != nil {
				ok = false
			}
			return netip.AddrPortFrom(a, uint16(p))
		}
		c.Proto, c.Client, c.VIP, c.Dest, c.State = f[0], ap(f[1], f[2]), ap(f[3], f[4]), ap(f[5], f[6]), f[7]
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// watchConns logs, while debugging, each connection IPVS forwards, once - the
// moment it is first seen, within a second: the client, the service and its
// VIP, and the backend it went to. Those in progress when this director began
// to lead are counted, not logged: another director forwarded them first.
func (s *session) watchConns(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var seen map[string]bool
	failed := ""
	for {
		conns, err := s.dp().Conns()
		if err != nil {
			if err.Error() != failed {
				s.log.Debug("reading IPVS's connections failed", "error", err)
			}
			failed = err.Error()
		} else {
			failed = ""
			services, backends := map[string]string{}, map[netip.Addr]string{}
			s.fsm.Read(func(st *cluster.State) {
				for _, v := range st.Services {
					services[strings.ToUpper(v.Proto)+" "+v.VIP+":"+strconv.Itoa(v.Port)] = v.Name
				}
				for _, n := range st.Nodes {
					if a, err := netip.ParseAddr(n.Address); err == nil {
						backends[a] = n.Name
					}
				}
			})
			now := make(map[string]bool, len(conns))
			for _, c := range conns {
				key := fmt.Sprint(c.Proto, c.Client, c.VIP, c.Dest)
				now[key] = true
				if seen == nil || seen[key] {
					continue
				}
				svc := services[c.Proto+" "+c.VIP.String()]
				if svc == "" {
					svc = "-"
				}
				node := backends[c.Dest.Addr()]
				if node == "" {
					node = "-"
				}
				s.log.Debug("connection forwarded", "client", c.Client, "service", svc, "vip", c.VIP, "backend", node, "to", c.Dest, "proto", strings.ToLower(c.Proto), "state", c.State)
			}
			if seen == nil {
				s.log.Debug("watching IPVS's connections, to log each new one", "in_progress", len(conns))
			}
			seen = now
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
