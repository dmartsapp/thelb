package director

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/dmartsapp/thelb/internal/ca"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/join"
	"github.com/dmartsapp/thelb/internal/wire"
)

// create creates a cluster: an ID, a CA, the director's own key and
// certificate - all in memory - and a Raft configuration of itself alone. The
// cluster's state is written when it first leads.
func (n *node) create() (*session, error) {
	now := n.d.Now()
	id, err := randomHex(4)
	if err != nil {
		return nil, err
	}
	authority, err := ca.New(n.cfg.Cluster, now)
	if err != nil {
		return nil, err
	}
	key, err := ca.NewKey()
	if err != nil {
		return nil, err
	}
	cert, der, err := authority.Sign("director", n.cfg.Name, []netip.Addr{n.addr.Prefix.Addr(), n.cfg.ControlVIP}, &key.PublicKey, now, ca.NodeLifetime)
	if err != nil {
		return nil, err
	}
	keyDER, err := authority.KeyDER()
	if err != nil {
		return nil, err
	}
	raftID, err := n.memberID()
	if err != nil {
		return nil, err
	}
	creating := &cluster.Create{
		Info: cluster.Info{Name: n.cfg.Cluster, ID: id, Created: now.UTC(), CACert: authority.DER, CAKey: keyDER},
		Node: cluster.Node{Claim: n.claim, ApprovedBy: "bootstrap", ApprovedAt: now.UTC(), Admitted: now.UTC()},
		Cert: certRecord(n.cfg.Name, "director", cert),
	}
	n.log.OK("creating the cluster: nothing answers on the control VIP", "cluster", n.cfg.Cluster, "vip", n.cfg.ControlVIP, "port", n.cfg.ControlPort)
	return newSession(n, raftID, key, der, authority.Cert, creating)
}

// memberID is a new member identity: the node's name and a random suffix.
func (n *node) memberID() (string, error) {
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}
	return n.cfg.Name + "-" + suffix, nil
}

// join asks the cluster on the control VIP to let this director in, and waits
// - for approval, if it needs one - until it is a member. It returns nil and no
// error if ctx ends first.
func (n *node) join(ctx context.Context) (*session, error) {
	key, err := ca.NewKey()
	if err != nil {
		return nil, err
	}
	csr, err := ca.Request(key, n.cfg.Name)
	if err != nil {
		return nil, err
	}
	n.setPhase("joining")
	said := map[string]time.Time{}
	say := func(warn bool, msg string, kv ...any) {
		if time.Since(said[msg]) < time.Minute {
			return
		}
		said[msg] = time.Now()
		if warn {
			n.log.Warn(msg, kv...)
		} else {
			n.log.OK(msg, kv...)
		}
	}
	var silentSince time.Time
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, nil
			case <-time.After(joinRetry):
			}
		}
		s, err := n.joinOnce(ctx, key, csr, say)
		switch {
		case ctx.Err() != nil:
			if s != nil {
				s.close(false)
			}
			return nil, nil
		case err == nil:
			return s, nil
		case errors.As(err, new(join.Stop)):
			return nil, err
		case errors.Is(err, join.ErrNoAnswer):
			// The cluster that answered is gone. A director that may create one
			// looks again, as at its start; one that may not keeps asking.
			if silentSince.IsZero() {
				silentSince = time.Now()
			}
			if n.cfg.Bootstrap && time.Since(silentSince) > 2*probeWindow {
				return nil, fmt.Errorf("%w: the cluster stopped answering on the control VIP", errRejoin)
			}
			say(true, "joining the cluster: trying again", "error", err)
		default:
			silentSince = time.Time{}
			say(true, "joining the cluster: trying again", "error", err)
		}
	}
}

func (n *node) joinOnce(ctx context.Context, key *ecdsa.PrivateKey, csr []byte, say func(bool, string, ...any)) (*session, error) {
	n.mu.Lock()
	dirSecret := n.dirSecret
	n.mu.Unlock()
	a := join.Asker{
		VIP:        netip.AddrPortFrom(n.cfg.ControlVIP, n.cfg.ControlPort),
		Role:       "director",
		Secret:     dirSecret,
		SecretFile: n.cfg.SecretFile,
		Claim:      n.claim,
		CSR:        csr,
		Dial:       n.net.DialVIP,
		Proven: func(cl wire.Cluster, id string) {
			n.mu.Lock()
			n.joining, n.joinID = &cl, id
			n.mu.Unlock()
		},
		Waiting: func() {
			n.setPhase("waiting for approval")
			n.mu.Lock()
			cl := n.joining
			n.mu.Unlock()
			say(false, "waiting for approval", "cluster", cl.Name, "node", n.cfg.Name, "fingerprint", n.claim.Fingerprint, "address", n.claim.Address, "mac", n.claim.MAC)
		},
	}
	ap, err := a.Ask(ctx)
	if err != nil {
		return nil, err
	}
	defer ap.Close()
	if ap.Before {
		n.log.OK("approved before: rejoining", "cluster", ap.Cluster.Name, "approved_by", ap.By)
	} else {
		n.log.OK("approved", "cluster", ap.Cluster.Name, "by", ap.By)
	}
	raftID, err := n.memberID()
	if err != nil {
		return nil, err
	}
	s, err := newSession(n, raftID, key, ap.Cert, ap.CA, nil)
	if err != nil {
		return nil, err
	}
	if _, err := ap.Ready(raftID, s.addr); err != nil {
		s.close(false)
		return nil, err
	}
	n.log.OK("joined the cluster", "cluster", ap.Cluster.Name, "id", ap.ClusterID, "member", raftID, "leader", ap.Cluster.Leader)
	return s, nil
}
