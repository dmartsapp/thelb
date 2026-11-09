package director

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/join"
	"github.com/hashicorp/raft"
)

// Schedulers a service may use: IPVS's, by its names.
var schedulers = []string{"wrr", "rr", "wlc", "lc", "sh", "mh"}

// change makes an admin change, on the leader, for the director by. It checks
// what only the leader can - the network, the cluster as it is - and writes the
// entry; applying it checks the rest.
func (s *session) change(by string, c api.Change) (api.Result, error) {
	switch c.Op {
	case "approve":
		return s.approve(by, c.Names)
	case "refuse", "remove":
		if len(c.Names) != 1 {
			return api.Result{}, fmt.Errorf("%s one node at a time", c.Op)
		}
		if c.Op == "refuse" {
			if err := s.apply(cluster.TypeRefuse, by, cluster.Name{Name: c.Names[0]}); err != nil {
				return api.Result{}, err
			}
			s.log.OK("node refused", "node", c.Names[0], "by", by)
			return api.Result{Names: c.Names}, nil
		}
		return s.remove(by, c.Names[0])
	case "pool.add":
		return s.poolAdd(by, c)
	case "pool.remove":
		if err := s.apply(cluster.TypePoolRemove, by, cluster.Name{Name: c.Pool}); err != nil {
			return api.Result{}, err
		}
		s.log.OK("pool removed", "pool", c.Pool, "by", by)
		return api.Result{Message: "removed: " + c.Pool}, nil
	case "service.add":
		return s.serviceAdd(by, c)
	case "service.remove":
		if err := s.apply(cluster.TypeServiceRemove, by, cluster.Name{Name: c.Names[0]}); err != nil {
			return api.Result{}, err
		}
		s.log.OK("service removed", "service", c.Names[0], "by", by)
		return api.Result{Message: "removed: " + c.Names[0]}, nil
	case "assign":
		return s.assign(by, c)
	case "unassign":
		if len(c.Names) != 2 {
			return api.Result{}, errors.New("unassign <service> <node>")
		}
		if err := s.apply(cluster.TypeUnassign, by, cluster.Unassign{Service: c.Names[0], Node: c.Names[1]}); err != nil {
			return api.Result{}, err
		}
		s.log.OK("offer unassigned", "service", c.Names[0], "node", c.Names[1], "by", by)
		return api.Result{Message: fmt.Sprintf("unassigned: %s from %s", c.Names[1], c.Names[0])}, nil
	}
	return api.Result{}, fmt.Errorf("no such change: %q", c.Op)
}

func (s *session) approve(by string, names []string) (api.Result, error) {
	if len(names) == 0 {
		return api.Result{}, errors.New("name a node")
	}
	var missing []string
	s.fsm.Read(func(st *cluster.State) {
		for _, name := range names {
			if st.Pending[name] == nil {
				missing = append(missing, name)
			}
		}
	})
	if len(missing) > 0 {
		return api.Result{}, fmt.Errorf("not waiting for approval: %v", missing)
	}
	if err := s.apply(cluster.TypeApprove, by, cluster.Names{Names: names}); err != nil {
		return api.Result{}, err
	}
	for _, name := range names {
		s.log.OK("node approved", "node", name, "by", by)
	}
	return api.Result{Names: names}, nil
}

func (s *session) remove(by, name string) (api.Result, error) {
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
		return api.Result{}, fmt.Errorf("%s is not a node of this cluster", name)
	}
	if n.Role == "director" && directors == 1 {
		return api.Result{}, fmt.Errorf("%s is the cluster's only director: stop it instead", name)
	}
	if err := s.apply(cluster.TypeRemove, by, cluster.Name{Name: name}); err != nil {
		return api.Result{}, err
	}
	s.log.OK("node removed", "node", name, "role", n.Role, "by", by)
	if n.Role == "director" && n.RaftID != "" {
		if err := s.r.RemoveServer(raft.ServerID(n.RaftID), 0, 5*time.Second).Error(); err != nil {
			s.log.Warn("removing the member from Raft failed", "node", name, "member", n.RaftID, "error", err)
		}
	}
	return api.Result{Names: []string{name}}, nil
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// poolAdd registers a pool: its first address and prefix, how many addresses,
// on an interface this director has in that network.
func (s *session) poolAdd(by string, c api.Change) (api.Result, error) {
	if !nameRE.MatchString(c.Pool) {
		return api.Result{}, fmt.Errorf("a pool's name is lowercase letters, digits and hyphens: %q", c.Pool)
	}
	p, err := netip.ParsePrefix(c.VIP)
	if err != nil {
		return api.Result{}, fmt.Errorf("--vip is the pool's first address and its network's prefix, as 10.10.1.20/24: %q", c.VIP)
	}
	first := p.Addr().Unmap()
	network := p.Masked()
	if c.Count < 1 || c.Count > 4096 {
		return api.Result{}, errors.New("--vippool-count is 1 to 4096")
	}
	last := first
	for i := 1; i < c.Count; i++ {
		last = last.Next()
	}
	if !network.Contains(first) || !network.Contains(last) {
		return api.Result{}, fmt.Errorf("%d addresses from %s do not fit in %s", c.Count, first, network)
	}
	if first.Is4() && network.Bits() < 31 {
		bcast := lastAddr(network)
		if first == network.Addr() || last == bcast || (first.Less(bcast) && bcast.Less(last)) {
			return api.Result{}, fmt.Errorf("the pool %s - %s holds %s's network or broadcast address", first, last, network)
		}
	}
	in := func(a netip.Addr) bool { return !a.Less(first) && !last.Less(a) }
	var problem string
	s.fsm.Read(func(st *cluster.State) {
		for _, o := range st.Pools {
			of, _ := netip.ParseAddr(o.First)
			ol := of
			for i := 1; i < o.Count; i++ {
				ol = ol.Next()
			}
			if of.BitLen() == first.BitLen() && !last.Less(of) && !ol.Less(first) {
				problem = fmt.Sprintf("it overlaps the pool %s", o.Name)
				return
			}
			if c.Default && o.Default && of.BitLen() == first.BitLen() {
				problem = fmt.Sprintf("the pool %s is the default already", o.Name)
				return
			}
		}
		for _, n := range st.Nodes {
			if a, err := netip.ParseAddr(n.Address); err == nil && in(a) {
				problem = fmt.Sprintf("it holds %s's address, %s", n.Name, a)
				return
			}
		}
	})
	if problem == "" && in(s.n.cfg.ControlVIP) {
		problem = "it holds the control VIP"
	}
	if problem != "" {
		return api.Result{}, errors.New(problem)
	}
	ifc, err := s.n.local(first, c.Interface)
	if err != nil {
		return api.Result{}, fmt.Errorf("this director cannot hold the pool's VIPs: %w", err)
	}
	if ifc.Prefix.Masked() != network {
		return api.Result{}, fmt.Errorf("%s's network is %s, not %s", ifc.Interface, ifc.Prefix.Masked(), network)
	}
	pool := cluster.Pool{Name: c.Pool, First: first.String(), Bits: network.Bits(), Count: c.Count, Interface: c.Interface, Default: c.Default}
	if err := s.apply(cluster.TypePoolAdd, by, pool); err != nil {
		return api.Result{}, err
	}
	s.log.OK("pool added", "pool", c.Pool, "first", first, "last", last, "network", network, "interface", ifc.Interface, "by", by)
	return api.Result{Message: fmt.Sprintf("pool %s: %s - %s, on %s", c.Pool, first, last, ifc.Interface)}, nil
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// serviceAdd creates a service: a VIP - the lowest free address of its pool,
// probed on the network first, or the one asked for - a protocol and a port.
func (s *session) serviceAdd(by string, c api.Change) (api.Result, error) {
	name := ""
	if len(c.Names) == 1 {
		name = c.Names[0]
	}
	if !nameRE.MatchString(name) {
		return api.Result{}, fmt.Errorf("a service's name is lowercase letters, digits and hyphens: %q", name)
	}
	if c.Proto != "tcp" && c.Proto != "udp" && c.Proto != "sctp" {
		return api.Result{}, fmt.Errorf("--proto is tcp, udp or sctp, not %q", c.Proto)
	}
	if c.Port < 1 || c.Port > 65535 {
		return api.Result{}, errors.New("--port is 1 to 65535")
	}
	sched := c.Scheduler
	if sched == "" {
		sched = "wrr"
	}
	if !slices.Contains(schedulers, sched) {
		return api.Result{}, fmt.Errorf("--scheduler is one of %s", strings.Join(schedulers, ", "))
	}
	var pool *cluster.Pool
	used := map[netip.Addr]bool{}
	taken := ""
	var want netip.Addr
	if c.VIP != "" {
		a, err := netip.ParseAddr(c.VIP)
		if err != nil {
			return api.Result{}, fmt.Errorf("--vip %q is not an address", c.VIP)
		}
		want = a.Unmap()
	}
	s.fsm.Read(func(st *cluster.State) {
		if c.Pool != "" {
			if p := st.Pools[c.Pool]; p != nil {
				cp := *p
				pool = &cp
			}
		} else {
			for _, p := range st.Pools {
				if p.Default && (!want.IsValid() || p.Contains(want)) {
					cp := *p
					pool = &cp
				}
			}
		}
		for _, v := range st.Services {
			a, _ := netip.ParseAddr(v.VIP)
			used[a] = true
			if a == want && v.Proto == c.Proto && v.Port == c.Port {
				taken = v.Name
			}
		}
	})
	if pool == nil {
		if c.Pool != "" {
			return api.Result{}, fmt.Errorf("no pool named %s", c.Pool)
		}
		return api.Result{}, errors.New("name a pool with --pool: there is no default pool")
	}
	if taken != "" {
		return api.Result{}, fmt.Errorf("%s %s/%d is the service %s's", want, c.Proto, c.Port, taken)
	}
	if want.IsValid() {
		if !pool.Contains(want) {
			return api.Result{}, fmt.Errorf("%s is not one of the pool %s's addresses", want, pool.Name)
		}
		if !used[want] {
			if who := s.probe(want); who != "" {
				return api.Result{}, fmt.Errorf("%s is in use on the network, by %s", want, who)
			}
		}
	} else {
		first, _ := netip.ParseAddr(pool.First)
		for i, a := 0, first; i < pool.Count; i, a = i+1, a.Next() {
			if used[a] {
				continue
			}
			if who := s.probe(a); who != "" {
				s.log.Warn("a pool's address is in use on the network: skipped", "pool", pool.Name, "vip", a, "by", who)
				continue
			}
			want = a
			break
		}
		if !want.IsValid() {
			return api.Result{}, fmt.Errorf("the pool %s has no free address", pool.Name)
		}
	}
	svc := cluster.Service{Name: name, Pool: pool.Name, VIP: want.String(), Proto: c.Proto, Port: c.Port, Scheduler: sched}
	if err := s.apply(cluster.TypeServiceAdd, by, svc); err != nil {
		return api.Result{}, err
	}
	s.log.OK("service added", "service", name, "vip", want, "proto", c.Proto, "port", c.Port, "scheduler", sched, "pool", pool.Name, "by", by)
	return api.Result{Message: fmt.Sprintf("service %s: %s %s/%d", name, want, c.Proto, c.Port)}, nil
}

// directorAt is whether a director has the address.
func directorAt(nodes map[string]cluster.Node, addr string) bool {
	for _, n := range nodes {
		if n.Role == "director" && n.Address == addr {
			return true
		}
	}
	return false
}

// probe asks the network whether anything has addr: "" when nothing answers.
func (s *session) probe(addr netip.Addr) string {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	who, err := s.dp().Probe(ctx, addr)
	if err != nil {
		s.log.Warn("probing an address failed", "address", addr, "error", err)
		return ""
	}
	return who
}

// assign assigns backends' offers - node:proto/port - to a service, after the
// checks the leader can make: the offer is the backend's, its protocol the
// service's, and the director reaches the backend directly, on a network of
// its own. An offer's port may differ from the service's: the backend maps it.
func (s *session) assign(by string, c api.Change) (api.Result, error) {
	if len(c.Names) != 1 || len(c.Offers) == 0 {
		return api.Result{}, errors.New("assign a service one or more offers: assign <service> <node>:<proto>/<port>")
	}
	service := c.Names[0]
	var svc *cluster.Service
	nodes := map[string]cluster.Node{}
	offers := map[string][]cluster.Offer{}
	s.fsm.Read(func(st *cluster.State) {
		if v := st.Services[service]; v != nil {
			cp := *v
			svc = &cp
		}
		for name, n := range st.Nodes {
			nodes[name] = *n
		}
		for name, o := range st.Offers {
			offers[name] = append([]cluster.Offer(nil), o.Offers...)
		}
	})
	if svc == nil {
		return api.Result{}, fmt.Errorf("no service named %s", service)
	}
	var as []cluster.Assignment
	var names []string
	for _, spec := range c.Offers {
		node, offer, ok := strings.Cut(spec, ":")
		proto, portText, ok2 := strings.Cut(offer, "/")
		port, err := strconv.Atoi(portText)
		if !ok || !ok2 || err != nil || !join.NameOK(node) {
			return api.Result{}, fmt.Errorf("an offer is <node>:<proto>/<port>, as web-1:tcp/8080, not %q", spec)
		}
		n, found := nodes[node]
		if !found || n.Role != "backend" {
			return api.Result{}, fmt.Errorf("%s is not a backend of this cluster", node)
		}
		if !slices.ContainsFunc(offers[node], func(o cluster.Offer) bool { return o.Proto == proto && o.Port == port }) {
			return api.Result{}, fmt.Errorf("%s does not offer %s/%d", node, proto, port)
		}
		if proto != svc.Proto {
			return api.Result{}, fmt.Errorf("%s offers %s, and the service %s is %s", node, proto, service, svc.Proto)
		}
		addr, err := netip.ParseAddr(n.Address)
		if err != nil {
			return api.Result{}, err
		}
		if port != svc.Port && directorAt(nodes, n.Address) {
			return api.Result{}, fmt.Errorf("%s runs on a director, so its offer must be on the service's port, %d: while that director leads, a port mapping there would take the clients' packets before IPVS", node, svc.Port)
		}
		if _, err := s.n.local(addr, ""); err != nil && addr != s.n.addr.Prefix.Addr() {
			return api.Result{}, fmt.Errorf("the forward path to %s: this director has no interface in %s's network - direct routing needs one", node, addr)
		}
		w := 1
		if v, ok := c.Weights[node]; ok {
			if v < 0 || v > 65535 {
				return api.Result{}, fmt.Errorf("a weight is 0 to 65535, not %d", v)
			}
			w = v
		}
		as = append(as, cluster.Assignment{Service: service, Node: node, Proto: proto, Port: port, Weight: w})
		names = append(names, node)
	}
	for node := range c.Weights {
		if !slices.Contains(names, node) {
			return api.Result{}, fmt.Errorf("--weight %s: %s is not among the offers assigned", node, node)
		}
	}
	if err := s.apply(cluster.TypeAssign, by, cluster.Assign{Assignments: as}); err != nil {
		return api.Result{}, err
	}
	for _, a := range as {
		s.log.OK("offer assigned", "service", service, "node", a.Node, "offer", fmt.Sprintf("%s/%d", a.Proto, a.Port), "weight", a.Weight, "by", by)
	}
	return api.Result{Names: names, Message: fmt.Sprintf("assigned to %s: %s", service, strings.Join(names, ", "))}, nil
}
