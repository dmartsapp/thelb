package director

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/raft"

	"github.com/dmartsapp/thelb/internal/cluster"
)

// Timing of the leader's guard. Variables, so that tests can shorten them.
var (
	guardEvery = 2 * time.Second
	// conflictEvery is how often the leader asks the link who else has the
	// control VIP.
	conflictEvery = 5 * time.Second
	// goneAfter is how long a voter goes without checking in before the leader
	// looks whether it is gone. Short: a director that starts again is a new
	// member identity anyway, and replaces its old one.
	goneAfter = 10 * time.Second
)

// guard watches, while this director leads, for what Raft does not see:
// another machine answering for the control VIP, and voters that are gone.
func (s *session) guard(ctx context.Context) {
	t := time.NewTicker(guardEvery)
	defer t.Stop()
	var looked time.Time
	said := map[string]string{} // the last warning, by subject: each said once
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.n.net.SetLease(s.vipLease()) // as the cluster's state has it now
		if time.Since(looked) >= conflictEvery {
			looked = time.Now()
			if s.watchVIP(ctx, said) {
				return
			}
		}
		s.dropGone(ctx, said)
	}
}

// watchVIP asks the link whether another machine answers for the control VIP,
// and acts on what vipConflict decides. It reports whether this director stood
// down.
func (s *session) watchVIP(ctx context.Context, said map[string]string) bool {
	vctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	holder, err := s.n.net.VIPHolder(vctx)
	cancel()
	if err != nil || holder == "" || ctx.Err() != nil {
		delete(said, "vip")
		return false
	}
	p, left := s.peerByMAC(holder)
	alive := false
	if p != nil {
		portErr, _, _ := s.look(ctx, p.addr)
		alive = portErr == nil
	}
	standDown, why := vipConflict(holder, p, left, alive, s.raftID)
	if standDown {
		s.log.Warn("standing down", "vip", s.n.cfg.ControlVIP, "reason", why)
		s.finish(fmt.Errorf("%w: %s", errRejoin, why))
		return true
	}
	s.n.net.Announce()
	if said["vip"] != why {
		said["vip"] = why
		s.log.Warn("another machine answers for the control VIP", "vip", s.n.cfg.ControlVIP, "by", holder, "reason", why)
	}
	return false
}

// vipConflict decides what a leader does about a machine, holder, answering
// for the control VIP too: stand down (true), or announce the VIP again
// (false), and why. p is the director the MAC address belongs to, if any; left,
// whether it is the one a takeover left behind; alive, whether its control port
// accepts connections.
//
// Two directors that each stood alone while unable to see each other - a
// partition no gateway and no link showed - both lead, once they see each
// other again: the one whose member identity sorts later stands down, starts
// over, and waits for approval, as the order of their takeovers has it. A
// director whose theLB is not running - killed, the VIP left on its interface -
// and any other machine are answered by announcing the VIP again.
func vipConflict(holder string, p *peer, left, alive bool, self string) (bool, string) {
	switch {
	case p == nil:
		return false, holder + " is no director of this cluster: an address conflict; announced the VIP again"
	case !alive:
		return false, p.name + "'s machine answers for it, but its theLB does not - what a theLB killed leaves; announced the VIP again"
	case !left || p.raftID == "":
		return false, p.name + " answers for it, with its theLB running; announced the VIP again"
	case self > p.raftID:
		return true, "two leaders: " + p.name + " leads too - the two stood alone at once, unable to see each other - and its member identity sorts first; this director starts over, and waits for approval"
	}
	return false, "two leaders: " + p.name + " leads too - the two stood alone at once, unable to see each other - and this director's member identity sorts first: it keeps leading, and announced the VIP again; " + p.name + " stands down"
}

// peerByMAC is the director a MAC address belongs to - the one a takeover left
// behind (true), removed from the state by now, or one of the state's - or nil.
func (s *session) peerByMAC(mac string) (*peer, bool) {
	if l := s.left; l != nil && strings.EqualFold(l.mac, mac) {
		c := *l
		return &c, true
	}
	var p *peer
	s.fsm.Read(func(st *cluster.State) {
		for _, n := range st.Nodes {
			if n.Role != "director" || n.Name == s.n.cfg.Name || !strings.EqualFold(n.MAC, mac) {
				continue
			}
			addr := n.RaftAddr
			if addr == "" {
				addr = net.JoinHostPort(n.Address, strconv.Itoa(int(s.n.cfg.ControlPort)))
			}
			p = &peer{name: n.Name, mac: n.MAC, raftID: n.RaftID, addr: addr}
		}
	})
	return p, false
}

// dropGone takes a voter that has not checked in for goneAfter, and that the
// leader finds gone - its theLB not running, or its machine not answering -
// out of the vote, as a planned stop takes itself out: with three directors or
// more, the others keep their majority without it, and still have one when
// another goes. It stays approved, and rejoins without approval when it starts
// again. With two, a leader could not be leading without the other: that is
// the takeover's.
func (s *session) dropGone(ctx context.Context, said map[string]string) {
	f := s.r.GetConfiguration()
	if f.Error() != nil {
		return
	}
	var voters []raft.Server
	for _, srv := range f.Configuration().Servers {
		if srv.Suffrage == raft.Voter {
			voters = append(voters, srv)
		}
	}
	if len(voters) < 3 {
		return
	}
	cis := s.checkInsCopy()
	s.mu.Lock()
	since := s.vipSince
	s.mu.Unlock()
	for _, v := range voters {
		id := string(v.ID)
		if id == s.raftID {
			continue
		}
		var n cluster.Node
		found := false
		s.fsm.Read(func(st *cluster.State) {
			if p := st.NodeByRaftID(id); p != nil {
				n, found = *p, true
			}
		})
		if !found {
			continue // an old member identity: the sweep's
		}
		last := since
		if ci, ok := cis[n.Name]; ok && ci.RaftID == id {
			last = ci.At
		}
		if time.Since(last) < goneAfter {
			delete(said, "gone "+n.Name)
			continue
		}
		portErr, answers, err := s.look(ctx, string(v.Address))
		if err != nil || ctx.Err() != nil {
			continue
		}
		gone, why := isGone(n.Name, portErr, answers)
		if !gone {
			if said["gone "+n.Name] != why {
				said["gone "+n.Name] = why
				s.log.Warn("a director has not checked in, but may be alive: it keeps its vote", "node", n.Name, "reason", why)
			}
			continue
		}
		if err := s.r.RemoveServer(v.ID, 0, 5*time.Second).Error(); err != nil {
			s.log.Warn("taking a gone director out of the vote failed", "node", n.Name, "error", err)
			return
		}
		s.mu.Lock()
		s.checkIns[n.Name] = checkIn{Name: n.Name, RaftID: id, At: last, Version: n.Version, Gone: true}
		s.mu.Unlock()
		s.log.Warn("director gone: out of the vote, so the others keep their majority; it rejoins without approval when it starts again", "node", n.Name, "member", id, "voters", s.voters(), "reason", why)
		return // one at a time
	}
}
