package director

import (
	"sort"
	"time"

	"github.com/dmartsapp/thelb/internal/ca"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/status"
	"github.com/hashicorp/raft"
)

// status is the cluster as this director sees it: the replicated state, Raft's
// view of who leads and who votes, and - from the leader - who checked in
// when. It holds no secret material: the status types have no field for any.
func (s *session) status() status.Status {
	st := status.Status{View: "director"}
	_, leaderID := s.r.LeaderWithID()
	term := s.r.CurrentTerm()
	suffrage := map[string]raft.ServerSuffrage{}
	if f := s.r.GetConfiguration(); f.Error() == nil {
		for _, srv := range f.Configuration().Servers {
			suffrage[string(srv.ID)] = srv.Suffrage
		}
	}
	checkIns, known := s.leaderCheckIns()
	now := time.Now()

	s.fsm.Read(func(cs *cluster.State) {
		if cs.Cluster == nil {
			return
		}
		c := &status.Cluster{Name: cs.Cluster.Name, ID: cs.Cluster.ID, Created: cs.Cluster.Created, Term: &term}
		if n := cs.NodeByRaftID(string(leaderID)); n != nil {
			c.Leader = n.Name
		}
		if l := cs.Leader; l != nil && l.RaftID == string(leaderID) {
			c.LeaderSince = l.Since
			st.VIPs = append(st.VIPs, status.VIP{VIP: s.n.cfg.ControlVIP.String(), Purpose: "control", Interface: l.Interface, HeldBy: l.Name, Since: l.Since})
		}
		st.Cluster = c

		for _, name := range sortedNames(cs.Nodes) {
			n := cs.Nodes[name]
			row := status.Node{Node: name, Role: n.Role, Address: n.Address, Version: n.Version, NodeFingerprint: n.Fingerprint}
			_, member := suffrage[n.RaftID]
			ci, checked := checkIns[name]
			checked = checked && ci.RaftID == n.RaftID
			switch {
			case n.RaftID != "" && n.RaftID == string(leaderID):
				row.Role, row.State = "leader", "up"
			case !known:
				row.State = "unknown"
			case checked && now.Sub(ci.At) < liveFor && member:
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
	})
	sort.Slice(st.Pending, func(i, j int) bool { return st.Pending[i].WaitingSeconds > st.Pending[j].WaitingSeconds })
	return st
}
