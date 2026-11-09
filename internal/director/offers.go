package director

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/check"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/ipvs"
)

// offerCheckTimeout is how long the leader's check of an application offered
// may take.
var offerCheckTimeout = 2 * time.Second

// offer is a backend's offer of one of its applications, or its withdrawal,
// done by the leader. An application offered must first answer the leader's
// check - the offer's own check, from the leader - and is then recorded,
// unassigned, for an admin to assign to a service. The backend names its
// offers as it likes; it never chooses a service.
func (s *session) offer(node string, c api.Offer) (api.Result, error) {
	o := c.Offer
	switch c.Op {
	case "add":
		return s.addOffer(node, o)
	case "remove":
		var x cluster.Offer
		found := false
		s.fsm.Read(func(st *cluster.State) { x, found = st.OfferNamed(node, o.Name) })
		if !found {
			return api.Result{}, fmt.Errorf("%s offers nothing named %s", node, o.Name)
		}
		if err := s.apply(cluster.TypeOfferRemove, node, cluster.OfferRemove{Node: node, Name: o.Name}); err != nil {
			return api.Result{}, err
		}
		s.log.OK("backend withdrew an offer", "node", node, "offer", x.Name, "port", x.Key())
		return api.Result{Names: []string{x.Name}, Message: fmt.Sprintf("withdrawn: %s (%s)", x.Name, x.Key())}, nil
	}
	return api.Result{}, fmt.Errorf("an offer is added or removed, not %q", c.Op)
}

func (s *session) addOffer(node string, o cluster.Offer) (api.Result, error) {
	if !cluster.OfferNameOK(o.Name) {
		return api.Result{}, fmt.Errorf("an offer's name is letters, digits, dots, hyphens and underscores, up to 63, not %q", o.Name)
	}
	if _, err := ipvs.ProtoNumber(o.Proto); err != nil || o.Port < 1 || o.Port > 65535 {
		return api.Result{}, fmt.Errorf("an offer is <tcp|udp|sctp>/<port>, not %s/%d", o.Proto, o.Port)
	}
	spec, err := check.Parse(o.Check, o.Proto)
	if err != nil {
		return api.Result{}, err
	}
	o.Check, o.Since = spec, time.Time{}
	var n *cluster.Node
	var clash, same bool
	var clashName string
	s.fsm.Read(func(st *cluster.State) {
		if p := st.Nodes[node]; p != nil {
			cp := *p
			n = &cp
		}
		if x, ok := st.OfferAt(node, o.Proto, o.Port); ok && x.Name != o.Name {
			clash, clashName = true, x.Name
		}
		if x, ok := st.OfferNamed(node, o.Name); ok && x.Same(o) {
			same = true
		}
	})
	if n == nil || n.Role != "backend" {
		return api.Result{}, fmt.Errorf("%s is not a backend of this cluster", node)
	}
	if clash {
		return api.Result{}, fmt.Errorf("%s offers %s already, as %s: withdraw that first", node, o.Key(), clashName)
	}
	addr, err := netip.ParseAddr(n.Address)
	if err != nil {
		return api.Result{}, err
	}
	at := netip.AddrPortFrom(addr, uint16(o.Port))
	if same {
		return api.Result{Names: []string{o.Name}, Message: fmt.Sprintf("offer %s: %s at %s, in the cluster already", o.Name, o.Key(), at)}, nil
	}
	ctx, cancel := context.WithTimeout(s.ctx, offerCheckTimeout+time.Second)
	defer cancel()
	if err := check.Probe(ctx, o.Check, at, offerCheckTimeout); err != nil {
		return api.Result{}, fmt.Errorf("the leader's check of %s (%s) at %s fails: %v - offer it once it answers", o.Name, check.Describe(o.Check), at, err)
	}
	if err := s.apply(cluster.TypeOfferAdd, node, cluster.OfferAdd{Node: node, Offer: o}); err != nil {
		return api.Result{}, err
	}
	s.log.OK("backend offers", "node", node, "offer", o.Name, "at", at, "check", check.Describe(o.Check))
	checked := "it answers the leader's check (" + check.Describe(o.Check) + ")"
	if o.Check == "none" {
		checked = "not checked: its check is none"
	}
	return api.Result{Names: []string{o.Name}, Message: fmt.Sprintf("offer %s: %s at %s, unassigned; %s", o.Name, o.Key(), at, checked)}, nil
}
