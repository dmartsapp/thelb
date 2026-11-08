package director

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/version"
	"github.com/dmartsapp/thelb/internal/wire"
	"github.com/hashicorp/raft"
)

// The API between directors: one request and one answer per connection, except
// check-ins, which stream - one a second, each answered.
const (
	opCheckIn  = "checkin"
	opCheckIns = "checkins"
	opApprove  = "approve"
	opRefuse   = "refuse"
	opRemove   = "remove"
	opRenew    = "renew"
)

type (
	apiRequest struct {
		V       int      `json:"v"`
		Op      string   `json:"op"`
		Names   []string `json:"names,omitempty"`
		CSR     []byte   `json:"csr,omitempty"`
		RaftID  string   `json:"raft_id,omitempty"`
		Applied uint64   `json:"applied,omitempty"`
		Version string   `json:"version,omitempty"`
	}
	apiResponse struct {
		V        int       `json:"v"`
		OK       bool      `json:"ok"`
		Error    string    `json:"error,omitempty"`
		Names    []string  `json:"names,omitempty"`
		Cert     []byte    `json:"cert,omitempty"`
		CheckIns []checkIn `json:"check_ins,omitempty"`
	}
)

var errNotLeader = errors.New("this director does not lead: ask the leader")

// serveAPI answers another director. TLS has checked that it is one of this
// cluster's, with a certificate not revoked.
func (s *session) serveAPI(c *tls.Conn) {
	peer, ok := wire.PeerName(c)
	if !ok {
		return
	}
	w := wire.NewConn(c)
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var req apiRequest
	if w.Recv(&req) != nil {
		return
	}
	if req.V != wire.Version {
		_ = w.Send(apiResponse{V: wire.Version, Error: fmt.Sprintf("this director speaks version %d of the API, not %d", wire.Version, req.V)})
		return
	}
	approved := false
	s.fsm.Read(func(st *cluster.State) {
		n := st.Nodes[peer.Name]
		approved = n != nil && n.Role == "director"
	})
	if !approved {
		_ = w.Send(apiResponse{V: wire.Version, Error: peer.Name + " is not a director of this cluster"})
		return
	}
	if req.Op == opCheckIn {
		s.serveCheckIns(c, w, peer.Name, req)
		return
	}
	resp := apiResponse{V: wire.Version, OK: true}
	var err error
	switch {
	case s.leading() == nil:
		err = errNotLeader
	case req.Op == opCheckIns:
		for _, ci := range s.checkInsCopy() {
			resp.CheckIns = append(resp.CheckIns, ci)
		}
	case req.Op == opApprove || req.Op == opRefuse || req.Op == opRemove:
		resp.Names, err = s.change(peer.Name, req.Op, req.Names)
	case req.Op == opRenew:
		resp.Cert, err = s.sign(peer.Name, req.CSR)
	default:
		err = fmt.Errorf("this director does not know %q", req.Op)
	}
	if err != nil {
		resp = apiResponse{V: wire.Version, Error: err.Error()}
	}
	_ = w.Send(resp)
}

// serveCheckIns records a director's check-ins, while this director leads, and
// promotes a non-voter to voter once it has caught up with its admission.
func (s *session) serveCheckIns(c *tls.Conn, w *wire.Conn, name string, req apiRequest) {
	for {
		if s.leading() == nil {
			_ = w.Send(apiResponse{V: wire.Version, Error: errNotLeader.Error()})
			return
		}
		var n cluster.Node
		found := false
		s.fsm.Read(func(st *cluster.State) {
			if p := st.Nodes[name]; p != nil {
				n, found = *p, true
			}
		})
		if !found || n.RaftID != req.RaftID {
			_ = w.Send(apiResponse{V: wire.Version, Error: "that member identity is not " + name + "'s"})
			return
		}
		s.mu.Lock()
		s.checkIns[name] = checkIn{Name: name, RaftID: req.RaftID, At: time.Now(), Applied: req.Applied, Version: req.Version}
		s.mu.Unlock()
		if req.Applied >= n.AdmitIndex {
			s.promote(name, n)
		}
		if w.Send(apiResponse{V: wire.Version, OK: true}) != nil {
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(liveFor + 2*checkInEvery))
		if w.Recv(&req) != nil {
			return
		}
	}
}

// promote makes a caught-up non-voter a voter.
func (s *session) promote(name string, n cluster.Node) {
	f := s.r.GetConfiguration()
	if f.Error() != nil {
		return
	}
	for _, srv := range f.Configuration().Servers {
		if string(srv.ID) != n.RaftID || srv.Suffrage != raft.Nonvoter {
			continue
		}
		s.mu.Lock()
		if s.promoting[n.RaftID] {
			s.mu.Unlock()
			return
		}
		s.promoting[n.RaftID] = true
		s.mu.Unlock()
		go func() {
			defer func() {
				s.mu.Lock()
				delete(s.promoting, n.RaftID)
				s.mu.Unlock()
			}()
			if err := s.r.AddVoter(srv.ID, srv.Address, 0, 10*time.Second).Error(); err != nil {
				s.log.Warn("promoting a director to voter failed", "node", name, "error", err)
				return
			}
			s.log.OK("director promoted to voter", "node", name, "member", n.RaftID)
		}()
		return
	}
}

// checkInLoop checks in with the leader, every second, while this director
// does not lead.
func (s *session) checkInLoop() {
	for s.ctx.Err() == nil {
		addr, id := s.r.LeaderWithID()
		if id == "" || string(id) == s.raftID {
			s.sleep(checkInEvery)
			continue
		}
		tc, err := s.dialMember(string(addr), wire.ALPNAPI, 2*time.Second)
		if err != nil {
			s.sleep(checkInEvery)
			continue
		}
		w := wire.NewConn(tc)
		stop := context.AfterFunc(s.ctx, func() { _ = tc.Close() })
		for s.ctx.Err() == nil {
			if _, now := s.r.LeaderWithID(); now != id {
				break
			}
			req := apiRequest{V: wire.Version, Op: opCheckIn, RaftID: s.raftID, Applied: s.r.AppliedIndex(), Version: version.Version}
			if w.Send(req) != nil {
				break
			}
			_ = tc.SetReadDeadline(time.Now().Add(liveFor))
			var resp apiResponse
			if w.Recv(&resp) != nil || !resp.OK {
				break
			}
			s.sleep(checkInEvery)
		}
		stop()
		_ = tc.Close()
	}
}

func (s *session) sleep(d time.Duration) {
	select {
	case <-s.ctx.Done():
	case <-time.After(d):
	}
}

// callLeader asks the leader, under this director's identity.
func (s *session) callLeader(req apiRequest) (apiResponse, error) {
	addr, id := s.r.LeaderWithID()
	if id == "" {
		return apiResponse{}, errors.New("the cluster has no leader at the moment")
	}
	if string(id) == s.raftID {
		return apiResponse{}, errors.New("this director leads")
	}
	tc, err := s.dialMember(string(addr), wire.ALPNAPI, 3*time.Second)
	if err != nil {
		return apiResponse{}, fmt.Errorf("reaching the leader: %w", err)
	}
	defer func() { _ = tc.Close() }()
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	w := wire.NewConn(tc)
	req.V = wire.Version
	if err := w.Send(req); err != nil {
		return apiResponse{}, err
	}
	var resp apiResponse
	if err := w.Recv(&resp); err != nil {
		return apiResponse{}, fmt.Errorf("the leader's answer: %w", err)
	}
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// admin is `thelb admin approve|refuse|remove` on this director: done here when
// it leads, asked of the leader otherwise.
func (s *session) admin(op string, names []string) ([]string, error) {
	if s.leading() != nil {
		return s.change(s.n.cfg.Name, op, names)
	}
	resp, err := s.callLeader(apiRequest{Op: op, Names: names})
	return resp.Names, err
}

// change approves, refuses or removes nodes, on the leader, for the director by.
func (s *session) change(by, op string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, errors.New("name a node")
	}
	switch op {
	case opApprove:
		var missing []string
		s.fsm.Read(func(st *cluster.State) {
			for _, name := range names {
				if st.Pending[name] == nil {
					missing = append(missing, name)
				}
			}
		})
		if len(missing) > 0 {
			return nil, fmt.Errorf("not waiting for approval: %v", missing)
		}
		if err := s.apply(cluster.TypeApprove, by, cluster.Names{Names: names}); err != nil {
			return nil, err
		}
		for _, name := range names {
			s.log.OK("node approved", "node", name, "by", by)
		}
		return names, nil
	case opRefuse:
		if len(names) != 1 {
			return nil, errors.New("refuse one node at a time")
		}
		if err := s.apply(cluster.TypeRefuse, by, cluster.Name{Name: names[0]}); err != nil {
			return nil, err
		}
		s.log.OK("node refused", "node", names[0], "by", by)
		return names, nil
	case opRemove:
		if len(names) != 1 {
			return nil, errors.New("remove one node at a time")
		}
		name := names[0]
		var n *cluster.Node
		directors := 0
		s.fsm.Read(func(st *cluster.State) {
			if p := st.Nodes[name]; p != nil {
				c := *p
				n = &c
			}
			for _, x := range st.Nodes {
				if x.Role == "director" {
					directors++
				}
			}
		})
		if n == nil {
			return nil, fmt.Errorf("%s is not a node of this cluster", name)
		}
		if n.Role == "director" && directors == 1 {
			return nil, fmt.Errorf("%s is the cluster's only director: stop it instead", name)
		}
		if err := s.apply(cluster.TypeRemove, by, cluster.Name{Name: name}); err != nil {
			return nil, err
		}
		s.log.OK("node removed", "node", name, "by", by)
		if n.RaftID != "" {
			if err := s.r.RemoveServer(raft.ServerID(n.RaftID), 0, 5*time.Second).Error(); err != nil {
				s.log.Warn("removing the member from Raft failed", "node", name, "member", n.RaftID, "error", err)
			}
		}
		return names, nil
	default:
		return nil, fmt.Errorf("no such change: %q", op)
	}
}

// leaderCheckIns are the leader's check-ins: this director's own, or the
// leader's, asked; ok is false when the leader cannot be asked.
func (s *session) leaderCheckIns() (map[string]checkIn, bool) {
	if s.leading() != nil {
		return s.checkInsCopy(), true
	}
	resp, err := s.callLeader(apiRequest{Op: opCheckIns})
	if err != nil {
		return nil, false
	}
	m := map[string]checkIn{}
	for _, ci := range resp.CheckIns {
		m[ci.Name] = ci
	}
	return m, true
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
