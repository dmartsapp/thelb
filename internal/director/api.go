package director

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/version"
	"github.com/dmartsapp/thelb/internal/wire"
	"github.com/hashicorp/raft"
)

var errNotLeader = errors.New("this director does not lead: ask the leader")

// serveAPI answers a member. TLS has checked that it is one of this cluster's,
// with a certificate not revoked: a director, or - on the control VIP - a
// backend.
func (s *session) serveAPI(c *tls.Conn) {
	peer, ok := wire.PeerName(c)
	if !ok {
		return
	}
	w := wire.NewConn(c)
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var req api.Request
	if w.Recv(&req) != nil {
		return
	}
	fail := func(err error) { _ = w.Send(api.Response{V: wire.Version, Error: err.Error()}) }
	if req.V != wire.Version {
		fail(fmt.Errorf("this director speaks version %d of the API, not %d", wire.Version, req.V))
		return
	}
	member := false
	s.fsm.Read(func(st *cluster.State) {
		n := st.Nodes[peer.Name]
		member = n != nil && n.Role == peer.Role
	})
	if !member {
		if peer.Role == "backend" && req.Op == api.OpBackend {
			_ = w.Send(api.Response{V: wire.Version, OK: true, Orders: &api.Orders{Removed: true}})
			return
		}
		fail(fmt.Errorf("%s is not a %s of this cluster", peer.Name, peer.Role))
		return
	}
	if req.Op == api.OpBackend || req.Op == api.OpCheckIn {
		// A stream: no deadline for its whole life - the deadline set for the
		// first request covers writes too, and would cut it - but one per
		// message, below.
		_ = c.SetDeadline(time.Time{})
	}
	if peer.Role == "backend" {
		switch req.Op {
		case api.OpBackend:
			s.serveBackend(c, w, peer.Name, req)
		case api.OpRenew, api.OpOffer:
			s.answer(w, peer.Name, req)
		default:
			fail(fmt.Errorf("a backend may not %s", req.Op))
		}
		return
	}
	if req.Op == api.OpCheckIn {
		s.serveCheckIns(c, w, peer.Name, req)
		return
	}
	s.answer(w, peer.Name, req)
}

func (s *session) answer(w *wire.Conn, by string, req api.Request) {
	resp := api.Response{V: wire.Version, OK: true}
	var err error
	switch {
	case s.leading() == nil:
		err = errNotLeader
	case req.Op == api.OpLive:
		resp.CheckIns, resp.Live, resp.Own = s.liveNow()
	case req.Op == api.OpOffer && req.Offer != nil:
		var r api.Result
		r, err = s.offer(by, *req.Offer)
		resp.Result = &r
	case req.Op == api.OpChange && req.Change != nil:
		var r api.Result
		r, err = s.change(by, *req.Change)
		resp.Result = &r
	case req.Op == api.OpRenew:
		resp.Cert, err = s.sign(by, req.CSR)
	default:
		err = fmt.Errorf("this director does not know %q", req.Op)
	}
	if err != nil {
		resp = api.Response{V: wire.Version, Error: err.Error()}
	}
	_ = w.Send(resp)
}

// serveCheckIns records a director's check-ins, while this director leads, and
// promotes a non-voter to voter once it has caught up with its admission.
func (s *session) serveCheckIns(c *tls.Conn, w *wire.Conn, name string, req api.Request) {
	for {
		if s.leading() == nil {
			_ = w.Send(api.Response{V: wire.Version, Error: errNotLeader.Error()})
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
			_ = w.Send(api.Response{V: wire.Version, Error: "that member identity is not " + name + "'s"})
			return
		}
		s.mu.Lock()
		s.checkIns[name] = checkIn{Name: name, RaftID: req.RaftID, At: time.Now(), Applied: req.Applied, Version: req.Version}
		s.mu.Unlock()
		if req.Applied >= n.AdmitIndex {
			s.promote(name, n)
		}
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if w.Send(api.Response{V: wire.Version, OK: true}) != nil {
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
			req := api.Request{V: wire.Version, Op: api.OpCheckIn, RaftID: s.raftID, Applied: s.r.AppliedIndex(), Version: version.Version}
			if w.Send(req) != nil {
				break
			}
			_ = tc.SetReadDeadline(time.Now().Add(liveFor))
			var resp api.Response
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
func (s *session) callLeader(req api.Request) (api.Response, error) {
	addr, id := s.r.LeaderWithID()
	if id == "" {
		return api.Response{}, errors.New("the cluster has no leader at the moment")
	}
	if string(id) == s.raftID {
		return api.Response{}, errors.New("this director leads")
	}
	tc, err := s.dialMember(string(addr), wire.ALPNAPI, 3*time.Second)
	if err != nil {
		return api.Response{}, fmt.Errorf("reaching the leader: %w", err)
	}
	defer func() { _ = tc.Close() }()
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	w := wire.NewConn(tc)
	req.V = wire.Version
	if err := w.Send(req); err != nil {
		return api.Response{}, err
	}
	var resp api.Response
	if err := w.Recv(&resp); err != nil {
		return api.Response{}, fmt.Errorf("the leader's answer: %w", err)
	}
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// admin is `thelb admin` on this director: done here when it leads, asked of
// the leader otherwise.
func (s *session) admin(c api.Change) (api.Result, error) {
	if s.leading() != nil {
		return s.change(s.n.cfg.Name, c)
	}
	resp, err := s.callLeader(api.Request{Op: api.OpChange, Change: &c})
	if err != nil {
		return api.Result{}, err
	}
	if resp.Result == nil {
		return api.Result{}, nil
	}
	return *resp.Result, nil
}

// leaderView is what only the leader knows: check-ins and heartbeats, each
// assignment's checks, weight and counters, and the backends' own checks of
// their offers.
type leaderView struct {
	checkIns map[string]checkIn
	live     map[string]api.Live // by assignment key
	own      map[string]string   // by node and offer key ("web-1 tcp/80"): ok or fail
	known    bool                // false: the leader could not be asked
}

// leaderLive is the leader's view: this director's own when it leads, asked of
// the leader otherwise.
func (s *session) leaderLive() leaderView {
	var cis []api.CheckIn
	var live []api.Live
	var own []api.Own
	if s.leading() != nil {
		cis, live, own = s.liveNow()
	} else {
		resp, err := s.callLeader(api.Request{Op: api.OpLive})
		if err != nil {
			return leaderView{}
		}
		cis, live, own = resp.CheckIns, resp.Live, resp.Own
	}
	v := leaderView{checkIns: map[string]checkIn{}, live: map[string]api.Live{}, own: map[string]string{}, known: true}
	for _, ci := range cis {
		v.checkIns[ci.Name] = checkIn(ci)
	}
	for _, x := range live {
		v.live[cluster.AssignmentKey(x.Service, x.Node)] = x
	}
	for _, x := range own {
		v.own[x.Node+" "+x.Offer] = x.Check
	}
	return v
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
