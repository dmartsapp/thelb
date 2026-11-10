package director

import (
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/ca"
	"github.com/dmartsapp/thelb/internal/check"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/status"
	"github.com/hashicorp/raft"
)

// status is the cluster as this director sees it: the replicated state, Raft's
// view of who leads and who votes, and - from the leader - check-ins,
// heartbeats, checks and counters. It holds no secret material: the status
// types have no field for any.
func (s *session) status(brief bool) status.Status {
	st := status.Status{View: "director"}
	_, leaderID := s.r.LeaderWithID()
	term := s.r.CurrentTerm()
	suffrage := map[string]raft.ServerSuffrage{}
	configured := false
	if f := s.r.GetConfiguration(); f.Error() == nil {
		configured = true
		for _, srv := range f.Configuration().Servers {
			suffrage[string(srv.ID)] = srv.Suffrage
		}
	}
	lv := s.leaderLive(brief)
	checkIns, live, known := lv.checkIns, lv.live, lv.known
	now := time.Now()

	s.fsm.Read(func(cs *cluster.State) {
		if cs.Cluster == nil {
			return
		}
		c := &status.Cluster{Name: cs.Cluster.Name, ID: cs.Cluster.ID, Created: cs.Cluster.Created, Term: &term, Lease: int(cs.VIPLease() / time.Second)}
		if n := cs.NodeByRaftID(string(leaderID)); n != nil {
			c.Leader = n.Name
		}
		var leaderSince time.Time
		if l := cs.Leader; l != nil && l.RaftID == string(leaderID) {
			c.LeaderSince, leaderSince = l.Since, l.Since
			st.VIPs = append(st.VIPs, status.VIP{VIP: s.n.cfg.ControlVIP.String(), Purpose: "control", Interface: l.Interface, HeldBy: l.Name, Since: l.Since, Port: int(s.n.cfg.ControlPort)})
		}
		st.Cluster = c

		for _, name := range sortedNames(cs.Nodes) {
			n := cs.Nodes[name]
			row := status.Node{Node: name, Role: n.Role, Address: n.Address, Version: n.Version, NodeFingerprint: n.Fingerprint, MAC: n.MAC}
			ci, checked := checkIns[name]
			checked = checked && ci.RaftID == n.RaftID
			lease := liveFor
			if n.Role == "backend" {
				lease = backendLease
			}
			_, member := suffrage[n.RaftID]
			switch {
			case n.RaftID != "" && n.RaftID == string(leaderID):
				row.Role, row.State = "leader", "up"
			case !known:
				row.State = "unknown"
			case configured && n.Role == "director" && n.RaftID != "" && !member && checked && ci.Gone:
				row.State = "down" // found gone by the leader, out of the vote
			case configured && n.Role == "director" && n.RaftID != "" && !member:
				row.State = "stopped" // stopped by plan, out of the vote
			case checked && now.Sub(ci.At) < lease && (member || n.Role == "backend"):
				row.State = "up"
				if suffrage[n.RaftID] == raft.Nonvoter {
					row.State = "catching up"
				}
			default:
				row.State = "down"
			}
			if checked {
				at := ci.At
				row.LastCheckIn = &at
				if ci.Version != "" {
					row.Version = ci.Version
				}
			}
			st.Nodes = append(st.Nodes, row)
		}

		st.Certificates = append(st.Certificates, status.Certificate{Certificate: s.id.CA.Subject.CommonName, Kind: "ca", Fingerprint: ca.Fingerprint(s.id.CA.Raw), NotAfter: s.id.CA.NotAfter})
		for _, name := range sortedNames(cs.Certs) {
			c := cs.Certs[name]
			st.Certificates = append(st.Certificates, status.Certificate{Certificate: c.Name, Kind: c.Kind, Fingerprint: c.Fingerprint, NotAfter: c.NotAfter})
		}
		for _, p := range cs.Pending {
			st.Pending = append(st.Pending, status.Pending{Pending: p.Name, Role: p.Role, Address: p.Address, MAC: p.MAC, NodeFingerprint: p.Fingerprint, WaitingSeconds: now.Sub(p.Since).Seconds()})
		}

		// Pools, and the VIPs their services hold.
		poolIfc := map[string]string{}
		for _, name := range sortedNames(cs.Pools) {
			p := cs.Pools[name]
			first, _ := netip.ParseAddr(p.First)
			ifc := "-"
			if a, err := s.n.local(first, p.Interface); err == nil {
				ifc = a.Interface
			}
			poolIfc[name] = ifc
			used := map[string]bool{}
			for _, v := range cs.Services {
				if v.Pool == name {
					used[v.VIP] = true
				}
			}
			st.Pools = append(st.Pools, status.Pool{Pool: name, Network: netip.PrefixFrom(first, p.Bits).Masked().String(), First: p.First, Count: p.Count, Interface: ifc, Used: len(used)})
		}
		seen := map[string]bool{}
		var services []*cluster.Service
		for _, v := range cs.Services {
			services = append(services, v)
		}
		sort.Slice(services, func(i, j int) bool {
			if services[i].VIP != services[j].VIP {
				return services[i].VIP < services[j].VIP
			}
			return services[i].Name < services[j].Name
		})
		// The leader holds a service VIP while a backend serves one of its
		// services, and only then.
		serving := map[string]bool{}
		for _, a := range cs.Assignments {
			if v := cs.Services[a.Service]; v != nil && a.Healthy && a.Weight > 0 {
				serving[v.VIP] = true
			}
		}
		for _, v := range services {
			if seen[v.VIP] {
				continue
			}
			seen[v.VIP] = true
			row := status.VIP{VIP: v.VIP, Purpose: "service", Pool: v.Pool, Interface: poolIfc[v.Pool], HeldBy: c.Leader, Since: v.At}
			if leaderSince.After(v.At) {
				row.Since = leaderSince
			}
			if c.Leader == "" || !serving[v.VIP] {
				row.HeldBy, row.Since = "-", time.Time{}
			}
			st.VIPs = append(st.VIPs, row)
		}

		// Offers, and the services they are assigned to.
		for _, name := range sortedNames(cs.Offers) {
			o := cs.Offers[name]
			n := cs.Nodes[name]
			if n == nil {
				continue
			}
			for _, off := range o.Offers {
				since := off.Since
				if since.IsZero() {
					since = o.Since
				}
				own := "-"
				if w, ok := lv.own[name+" "+off.Key()]; ok {
					own = w
				}
				row := status.Offer{Node: name, Offer: off.Name, Address: n.Address, Protocol: off.Proto, Port: off.Port, Check: check.Describe(off.Check), OwnCheck: own, SinceSeconds: now.Sub(since).Seconds()}
				var to []string
				for _, a := range cs.Assignments {
					if a.Node == name && a.Proto == off.Proto && a.Port == off.Port {
						to = append(to, a.Service)
					}
				}
				if len(to) > 0 {
					sort.Strings(to)
					svc := strings.Join(to, ", ")
					row.Service = &svc
				}
				st.Offers = append(st.Offers, row)
			}
		}

		// Services, and the backends serving them.
		for _, v := range services {
			m := "direct"
			if v.Tunnel() {
				m = "tunnel"
			}
			row := status.Service{Service: v.Name, VIP: v.VIP, Protocol: v.Proto, Port: v.Port, Scheduler: v.Scheduler, Method: m}
			for _, key := range sortedNames(cs.Assignments) {
				a := cs.Assignments[key]
				if a.Service != v.Name {
					continue
				}
				row.Backends++
				n := cs.Nodes[a.Node]
				l, ok := live[key]
				if !ok {
					l = api.Live{Own: "-", Director: "-"}
					if a.Healthy {
						l.Weight = a.Weight
					}
				}
				if l.Weight > 0 || (!known && a.Healthy) {
					row.BackendsUp++
				}
				b := status.Backend{Service: v.Name, VIP: v.VIP, Port: v.Port, Backend: a.Node, AppPort: a.Port, OwnCheck: l.Own, DirectorCheck: l.Director,
					Weight: l.Weight, Connections: l.Conns, Packets: l.Packets, Bytes: l.Bytes, LastCheckInSeconds: -1}
				if n != nil {
					b.Address = n.Address
				}
				if ci, ok := checkIns[a.Node]; ok {
					b.LastCheckInSeconds = now.Sub(ci.At).Seconds()
				}
				st.Backends = append(st.Backends, b)
			}
			st.Services = append(st.Services, row)
		}
	})
	sort.Slice(st.Pending, func(i, j int) bool { return st.Pending[i].WaitingSeconds > st.Pending[j].WaitingSeconds })
	return st
}
