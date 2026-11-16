package director

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/ipvs"
	"github.com/dmartsapp/thelb/internal/kube"
)

// The names theLB reads in a cluster.
const (
	kubeClass        = "dmarts.app/thelb"
	labelDirector    = "dmarts.app/thelb-director"
	labelExclude     = "node.kubernetes.io/exclude-from-external-load-balancers"
	annotationPool   = "dmarts.app/thelb-pool"
	annotationIP     = "dmarts.app/thelb-ip"
	annotationShare  = "dmarts.app/thelb-shared-vip"
	annotationCilium = "service.cilium.io/node-selector"
	ingressModeVIP   = "VIP"
)

// kubeReconcileEach is how often the leader goes over every Service, beside
// each change it hears of - unless Deps.KubeEvery says otherwise.
const kubeReconcileEach = 10 * time.Second

// kubeAccess is every permission a director serving Kubernetes needs - and
// no more: it reads, and writes a Service's status, never a Service.
var kubeAccess = []kube.Access{
	{Resource: "services", Verb: "get"}, {Resource: "services", Verb: "list"}, {Resource: "services", Verb: "watch"},
	{Resource: "services", Subresource: "status", Verb: "update"}, {Resource: "services", Subresource: "status", Verb: "patch"},
	{Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "get"}, {Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "list"}, {Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "watch"},
	{Resource: "nodes", Verb: "get"}, {Resource: "nodes", Verb: "list"}, {Resource: "nodes", Verb: "watch"},
	{Group: "events.k8s.io", Resource: "events", Verb: "create"}, {Group: "events.k8s.io", Resource: "events", Verb: "patch"},
}

// KubeAccess is every permission a director serving Kubernetes needs: for
// thelb generate k8s kubeconfig to check a token against.
func KubeAccess() []kube.Access { return slices.Clone(kubeAccess) }

// kubeView is what a director serving Kubernetes knows of the cluster: every
// director watches it, so a new leader knows it already; only the leader
// writes.
type kubeView struct {
	c        *kube.Client
	services *kube.Cache[kube.Service]
	nodes    *kube.Cache[kube.Node]
	slices   *kube.Cache[kube.EndpointSlice]
	server   string
}

func newKubeView(c *kube.Client) *kubeView {
	key := func(m kube.Meta) string { return m.Key() }
	return &kubeView{
		c:        c,
		services: kube.NewCache(c, "/api/v1/services", func(s *kube.Service) string { return key(s.Meta) }),
		nodes:    kube.NewCache(c, "/api/v1/nodes", func(n *kube.Node) string { return key(n.Meta) }),
		slices:   kube.NewCache(c, "/apis/discovery.k8s.io/v1/endpointslices", func(e *kube.EndpointSlice) string { return key(e.Meta) }),
		server:   c.Config().Server.String(),
	}
}

func (k *kubeView) run(ctx context.Context) {
	go k.services.Run(ctx)
	go k.nodes.Run(ctx)
	go k.slices.Run(ctx)
}

// changed is closed at the next change to any of the caches.
func (k *kubeView) changed(ctx context.Context) <-chan struct{} {
	out := make(chan struct{})
	a, b, c := k.services.Changed(), k.nodes.Changed(), k.slices.Changed()
	go func() {
		select {
		case <-a:
		case <-b:
		case <-c:
		case <-ctx.Done():
		}
		close(out)
	}()
	return out
}

// kubeStart checks that this director can serve Kubernetes - the API server
// answers, it is recent enough, every permission is held, and a director on
// a node of the cluster has that node labelled - then watches the cluster.
func (n *node) kubeStart(ctx context.Context, c *kube.Client, self netip.Addr) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	src := c.Config().Source
	minor, git, err := c.Version(cctx)
	if err != nil {
		return fmt.Errorf("the Kubernetes API server %s (%s) does not answer: %w", c.Config().Server, src, err)
	}
	if minor < kube.MinMinor {
		return fmt.Errorf("the Kubernetes API server runs %s: theLB serves Kubernetes 1.%d and later", git, kube.MinMinor)
	}
	var missing []string
	for _, a := range kubeAccess {
		ok, err := c.Allowed(cctx, a)
		if err != nil {
			return fmt.Errorf("asking the Kubernetes API server what this director may do: %w", err)
		}
		if !ok {
			missing = append(missing, a.String())
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the Kubernetes credential (%s) lacks permissions theLB needs: %s - apply what thelb generate k8s prints", src, strings.Join(missing, ", "))
	}
	nodes, _, err := kube.List[kube.Node](cctx, c, "/api/v1/nodes")
	if err != nil {
		return fmt.Errorf("listing the cluster's nodes: %w", err)
	}
	for _, kn := range nodes {
		for _, a := range kn.Status.Addresses {
			if ip, err := netip.ParseAddr(a.Address); err == nil && ip.Unmap() == self {
				if _, ok := kn.Meta.Labels[labelDirector]; !ok {
					return fmt.Errorf("this director runs on the Kubernetes node %s, which is not labelled as a director: the node's own service proxy would take the VIPs' packets before theLB - label it (kubectl label node %s %s=true), and keep theLB's Services off it as the documentation says", kn.Meta.Name, kn.Meta.Name, labelDirector)
				}
				n.log.OK("this director runs on a Kubernetes node labelled as a director", "node", kn.Meta.Name)
			}
		}
	}
	n.log.OK("serving Kubernetes Services of class "+kubeClass, "api", c.Config().Server.String(), "version", git, "credential", src)
	n.kube = newKubeView(c)
	n.kube.run(ctx)
	return nil
}

// eligible is whether a Service is theLB's to serve.
func eligible(s *kube.Service) bool { return s.Spec.Type == "LoadBalancer" && s.Class() == kubeClass }

// families are the IP families a Service asks for: IPv4 when it says none.
func families(s *kube.Service) []string {
	if len(s.Spec.IPFamilies) == 0 {
		return []string{"IPv4"}
	}
	return s.Spec.IPFamilies
}

func familyOf(a netip.Addr) string {
	if a.Is4() {
		return "IPv4"
	}
	return "IPv6"
}

func splitList(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// kubeState is what the controller reads of the replicated state.
type kubeState struct {
	pools map[string]cluster.Pool
	alloc map[string]cluster.KubeService
	owner func(vip string, except ...string) string
}

func (s *session) readKubeState() kubeState {
	ks := kubeState{pools: map[string]cluster.Pool{}, alloc: map[string]cluster.KubeService{}}
	vipOwner := map[string]string{} // a copy, for owner
	kubeOwners := map[string][]string{}
	s.fsm.Read(func(st *cluster.State) {
		for k, p := range st.Pools {
			ks.pools[k] = *p
		}
		for k, a := range st.Kube {
			cp := *a
			cp.VIPs = slices.Clone(a.VIPs)
			ks.alloc[k] = cp
			for _, x := range a.VIPs {
				kubeOwners[x.VIP] = append(kubeOwners[x.VIP], k)
			}
		}
		for _, v := range st.Services {
			vipOwner[v.VIP] = "the service " + v.Name
		}
	})
	ks.owner = func(vip string, except ...string) string {
		if o := vipOwner[vip]; o != "" {
			return o
		}
		for _, k := range kubeOwners[vip] {
			if !slices.Contains(except, k) {
				return "the Kubernetes Service " + k
			}
		}
		return ""
	}
	return ks
}

// kubeController gives theLB's Services their VIPs and says so in their
// status, and takes back the VIPs of Services that are gone, while this
// director leads.
func (s *session) kubeController(ctx context.Context) {
	k := s.n.kube
	said := map[string]string{} // by Service and reason: the last event or warning, so each is said once
	every := kubeReconcileEach
	if s.n.d.KubeEvery > 0 {
		every = s.n.d.KubeEvery
	}
	for {
		changed := k.changed(ctx)
		fsmChanged := s.fsm.Changed()
		s.kubeReconcile(ctx, said)
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-fsmChanged:
		case <-time.After(every):
		}
	}
}

// kubeReconcile is one pass of the controller.
func (s *session) kubeReconcile(ctx context.Context, said map[string]string) {
	k := s.n.kube
	if !k.services.Synced() {
		return // knows nothing yet: never takes back a VIP for a Service it has not seen
	}
	services := k.services.Items()
	directorNodes := false
	for _, kn := range k.nodes.Items() {
		if _, ok := kn.Meta.Labels[labelDirector]; ok {
			directorNodes = true
		}
	}
	ks := s.readKubeState()
	nodes, slices := k.nodes.Items(), k.slices.Items()
	directorAddrs := map[netip.Addr]bool{}
	s.fsm.Read(func(st *cluster.State) {
		for _, n := range st.Nodes {
			if a, err := netip.ParseAddr(n.Address); err == nil && n.Role == "director" {
				directorAddrs[a.Unmap()] = true
			}
		}
	})
	for _, key := range sortedNames(services) {
		svc := services[key]
		if !eligible(svc) {
			continue
		}
		want, problem := s.kubeAllocate(svc, ks)
		if problem != "" {
			s.kubeSay(ctx, said, svc, true, "AllocationFailed", "Allocate", problem)
			continue
		}
		old, had := ks.alloc[key]
		if !had || !sameAlloc(old, want) {
			if err := s.apply(cluster.TypeKubeAlloc, s.n.cfg.Name, want); err != nil {
				s.kubeSay(ctx, said, svc, true, "AllocationFailed", "Allocate", err.Error())
				continue
			}
			ks = s.readKubeState()
			var vips []string
			for _, x := range want.VIPs {
				vips = append(vips, x.VIP)
			}
			s.log.OK("Kubernetes Service given its VIPs", "service", key, "vips", strings.Join(vips, ","))
			delete(said, key+" AllocationFailed")
			s.kubeSay(ctx, said, svc, false, "IPAllocated", "Allocate", "theLB gave it "+strings.Join(vips, ", "))
		}
		if k.nodes.Synced() && k.slices.Synced() {
			if note := kubeExcludedPods(svc, nodes, slices, directorAddrs); note != "" {
				s.kubeSay(ctx, said, svc, true, "NodesExcluded", "Check", note)
			} else {
				delete(said, key+" NodesExcluded") // said again if it happens again
			}
		}
		if directorNodes && !strings.Contains(svc.Meta.Annotations[annotationCilium], labelDirector) {
			s.kubeSay(ctx, said, svc, true, "NoDirectorExclusion", "Check",
				fmt.Sprintf(`directors run on nodes labelled %s, and this Service lacks the annotation %s: "!%s" - with Cilium, the director nodes take its packets before theLB`, labelDirector, annotationCilium, labelDirector))
		}
		s.kubeStatus(ctx, svc, ks.alloc[key])
	}
	// What is gone - deleted, created again, or no longer theLB's - gives its
	// VIPs back.
	for _, key := range sortedNames(ks.alloc) {
		a := ks.alloc[key]
		svc := services[key]
		if svc != nil && svc.Meta.UID == a.UID && eligible(svc) {
			continue
		}
		if err := s.apply(cluster.TypeKubeRelease, s.n.cfg.Name, cluster.Name{Name: key}); err != nil {
			s.log.Warn("taking back a Kubernetes Service's VIPs failed", "service", key, "error", err)
			continue
		}
		s.log.OK("Kubernetes Service gone: its VIPs taken back", "service", key)
		for k := range said {
			if strings.HasPrefix(k, key+" ") {
				delete(said, k)
			}
		}
		if svc != nil && svc.Meta.UID == a.UID && ingressIs(svc, a) {
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			if err := k.c.SetIngress(cctx, svc.Meta.Namespace, svc.Meta.Name, nil); err != nil && !kube.IsNotFound(err) {
				s.log.Warn("clearing a Service's load-balancer status failed", "service", key, "error", err)
			}
			cancel()
		}
	}
}

// kubeStatus writes the Service's status.loadBalancer.ingress, if it differs.
func (s *session) kubeStatus(ctx context.Context, svc *kube.Service, a cluster.KubeService) {
	if ingressIs(svc, a) {
		return
	}
	var in []kube.Ingress
	for _, x := range a.VIPs {
		in = append(in, kube.Ingress{IP: x.VIP, IPMode: ingressModeVIP})
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.n.kube.c.SetIngress(cctx, svc.Meta.Namespace, svc.Meta.Name, in); err != nil {
		s.log.Warn("writing a Service's load-balancer status failed", "service", svc.Meta.Key(), "error", err)
	}
}

// ingressIs is whether the Service's status names exactly the allocation's
// VIPs, as theLB writes them.
func ingressIs(svc *kube.Service, a cluster.KubeService) bool {
	in := svc.Status.LoadBalancer.Ingress
	if len(in) != len(a.VIPs) {
		return false
	}
	for i, x := range a.VIPs {
		if in[i].IP != x.VIP || in[i].IPMode != ingressModeVIP {
			return false
		}
	}
	return true
}

func sameAlloc(a, b cluster.KubeService) bool {
	return a.Key == b.Key && a.UID == b.UID && a.Share == b.Share && slices.Equal(a.VIPs, b.VIPs)
}

// kubeSay logs a Service's news and writes it as an event - each reason only
// once, until its note changes.
func (s *session) kubeSay(ctx context.Context, said map[string]string, svc *kube.Service, warning bool, reason, action, note string) {
	key := svc.Meta.Key() + " " + reason
	if said[key] == note {
		return
	}
	said[key] = note
	if warning {
		s.log.Warn("Kubernetes Service: "+note, "service", key, "reason", reason)
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := s.n.kube.c.Emit(cctx, kube.Event{Service: svc.Meta, Warning: warning, Reason: reason, Action: action, Note: note, Instance: s.n.cfg.Name, Timestamp: time.Now()})
	if err != nil {
		s.log.Warn("writing an event about a Service failed", "service", key, "error", err)
	}
}

// kubeAllocate is the VIPs a Service is to have: those it has, while they
// still fit what it asks; else, for each of its families, the address it asks
// for, the VIP of a Service it shares with, or the lowest free address of its
// pool. Or why it cannot have them.
func (s *session) kubeAllocate(svc *kube.Service, ks kubeState) (cluster.KubeService, string) {
	key := svc.Meta.Key()
	want := cluster.KubeService{Key: key, UID: svc.Meta.UID, Share: svc.Meta.Annotations[annotationShare]}
	old, had := ks.alloc[key]
	if had && old.UID != svc.Meta.UID {
		had = false // a Service created again under the same name: a new one
	}
	poolNames := splitList(svc.Meta.Annotations[annotationPool])
	var asked []netip.Addr
	for _, x := range splitList(svc.Meta.Annotations[annotationIP]) {
		a, err := netip.ParseAddr(x)
		if err != nil {
			return want, fmt.Sprintf("%s %q is not an address", annotationIP, x)
		}
		asked = append(asked, a.Unmap())
	}
	for _, name := range poolNames {
		if _, ok := ks.pools[name]; !ok {
			return want, fmt.Sprintf("%s names %s, and no pool has that name", annotationPool, name)
		}
	}
	for _, fam := range families(svc) {
		var ask netip.Addr
		for _, a := range asked {
			if familyOf(a) == fam {
				ask = a
			}
		}
		pool, problem := kubePool(ks.pools, poolNames, ask, fam)
		if problem != "" {
			return want, problem
		}
		vip, problem := s.kubeVIP(svc, ks, pool, ask, fam, old, had, want.Share)
		if problem != "" {
			return want, problem
		}
		want.VIPs = append(want.VIPs, cluster.KubeVIP{Pool: pool.Name, VIP: vip.String()})
	}
	return want, ""
}

// kubePool is the pool a Service's VIP of family fam comes from: one it names,
// one holding the address it asks for, or the family's default.
func kubePool(pools map[string]cluster.Pool, names []string, ask netip.Addr, fam string) (cluster.Pool, string) {
	fits := func(p cluster.Pool) bool {
		first, err := netip.ParseAddr(p.First)
		return err == nil && familyOf(first) == fam && (!ask.IsValid() || p.Contains(ask))
	}
	if len(names) > 0 {
		for _, name := range names {
			if p := pools[name]; fits(p) {
				return p, ""
			}
		}
		if ask.IsValid() {
			return cluster.Pool{}, fmt.Sprintf("%s %s is in none of the pools %s names", annotationIP, ask, annotationPool)
		}
		return cluster.Pool{}, fmt.Sprintf("%s names no %s pool", annotationPool, fam)
	}
	for _, name := range sortedNames(pools) {
		p := pools[name]
		if ask.IsValid() && fits(p) {
			return p, ""
		}
		if !ask.IsValid() && p.Default && fits(p) {
			return p, ""
		}
	}
	if ask.IsValid() {
		return cluster.Pool{}, fmt.Sprintf("%s %s is in no pool", annotationIP, ask)
	}
	return cluster.Pool{}, fmt.Sprintf("no default %s pool: make one (thelb admin pool add ... --default), or name one with %s", fam, annotationPool)
}

// kubeVIP is the Service's VIP of one family, from pool.
func (s *session) kubeVIP(svc *kube.Service, ks kubeState, pool cluster.Pool, ask netip.Addr, fam string, old cluster.KubeService, had bool, share string) (netip.Addr, string) {
	key := svc.Meta.Key()
	if share != "" {
		for _, other := range sortedNames(ks.alloc) {
			o := ks.alloc[other]
			if other == key || o.Share != share {
				continue
			}
			for _, x := range o.VIPs {
				a, err := netip.ParseAddr(x.VIP)
				if err != nil || familyOf(a) != fam || x.Pool != pool.Name {
					continue
				}
				if ask.IsValid() && ask != a {
					return netip.Addr{}, fmt.Sprintf("%s %s is not %s, the VIP %s shares with it", annotationIP, ask, a, other)
				}
				if clash := s.portClash(svc, a, ks); clash != "" {
					return netip.Addr{}, clash
				}
				return a, ""
			}
		}
	}
	if had {
		for _, x := range old.VIPs {
			a, err := netip.ParseAddr(x.VIP)
			if err == nil && familyOf(a) == fam && x.Pool == pool.Name && (!ask.IsValid() || ask == a) {
				return a, ""
			}
		}
	}
	// The address its status names, which theLB wrote: the cluster's memory of
	// it after every director stopped - the allocations were in theirs only.
	if !had && !ask.IsValid() {
		for _, a := range statusVIPs(svc) {
			if familyOf(a) == fam && pool.Contains(a) && ks.owner(a.String(), key) == "" && s.probe(a) == "" {
				return a, ""
			}
		}
	}
	if ask.IsValid() {
		if owner := ks.owner(ask.String(), key); owner != "" {
			return netip.Addr{}, fmt.Sprintf("%s %s is %s's", annotationIP, ask, owner)
		}
		if who := s.probe(ask); who != "" {
			return netip.Addr{}, fmt.Sprintf("%s %s is in use on the network, by %s", annotationIP, ask, who)
		}
		return ask, ""
	}
	// The lowest free address - first passing over those other Services'
	// statuses name, which they take back once the controller reaches them.
	claimed := map[netip.Addr]bool{}
	for k, o := range s.n.kube.services.Items() {
		if k != key && eligible(o) {
			for _, a := range statusVIPs(o) {
				claimed[a] = true
			}
		}
	}
	first, _ := netip.ParseAddr(pool.First)
	for _, skipClaimed := range []bool{true, false} {
		for i, a := 0, first; i < pool.Count; i, a = i+1, a.Next() {
			if ks.owner(a.String(), key) != "" || (skipClaimed && claimed[a]) {
				continue
			}
			if who := s.probe(a); who != "" {
				s.log.Warn("a pool's address is in use on the network: skipped", "pool", pool.Name, "vip", a, "by", who)
				continue
			}
			return a, ""
		}
	}
	return netip.Addr{}, fmt.Sprintf("the pool %s has no free address", pool.Name)
}

// statusVIPs are the addresses a Service's status names that theLB wrote -
// with ipMode VIP.
func statusVIPs(svc *kube.Service) []netip.Addr {
	var out []netip.Addr
	for _, in := range svc.Status.LoadBalancer.Ingress {
		if a, err := netip.ParseAddr(in.IP); err == nil && in.IPMode == ingressModeVIP {
			out = append(out, a.Unmap())
		}
	}
	return out
}

// portClash is why the Service cannot share vip: a port another Service on it
// has, or "".
func (s *session) portClash(svc *kube.Service, vip netip.Addr, ks kubeState) string {
	mine := map[string]bool{}
	for _, p := range svc.Spec.Ports {
		mine[protoName(p.Protocol)+"/"+fmt.Sprint(p.Port)] = true
	}
	services := s.n.kube.services.Items()
	for _, other := range sortedNames(ks.alloc) {
		if other == svc.Meta.Key() {
			continue
		}
		on := false
		for _, x := range ks.alloc[other].VIPs {
			on = on || x.VIP == vip.String()
		}
		o := services[other]
		if !on || o == nil {
			continue
		}
		for _, p := range o.Spec.Ports {
			if k := protoName(p.Protocol) + "/" + fmt.Sprint(p.Port); mine[k] {
				return fmt.Sprintf("it shares %s with %s, which has the port %s already", vip, other, k)
			}
		}
	}
	return ""
}

// protoName is a Service port's protocol as theLB names it: tcp, udp, sctp.
func protoName(p string) string {
	if p == "" {
		return "tcp"
	}
	return strings.ToLower(p)
}

// kubeTarget is a node of the cluster that may serve theLB's Services: ready,
// not excluded, not a director's.
type kubeTarget struct {
	name  string
	addrs []netip.Addr // InternalIP first, then ExternalIP
}

// kubeTargets are the nodes that may serve, sorted by name.
func kubeTargets(nodes map[string]*kube.Node, directorAddrs map[netip.Addr]bool) []kubeTarget {
	var out []kubeTarget
	for _, name := range sortedNames(nodes) {
		if addrs, why := nodeExclusion(nodes[name], directorAddrs); why == "" {
			out = append(out, kubeTarget{name: name, addrs: addrs})
		}
	}
	return out
}

// nodeExclusion is a node's addresses, InternalIP first, and why theLB sends
// it no Service's traffic - "" when it does.
func nodeExclusion(kn *kube.Node, directorAddrs map[netip.Addr]bool) ([]netip.Addr, string) {
	name := kn.Meta.Name
	if !kn.Ready() {
		return nil, name + " is not Ready"
	}
	if _, ok := kn.Meta.Labels[labelExclude]; ok {
		return nil, fmt.Sprintf("%s has the label %s, which kubeadm puts on control-plane nodes; to send it traffic: kubectl label node %s %s-", name, labelExclude, name, labelExclude)
	}
	if _, ok := kn.Meta.Labels[labelDirector]; ok {
		return nil, fmt.Sprintf("%s is a director's node, labelled %s: taint it, so that no pods run there", name, labelDirector)
	}
	var addrs []netip.Addr
	for _, typ := range []string{"InternalIP", "ExternalIP"} {
		for _, a := range kn.Status.Addresses {
			ip, err := netip.ParseAddr(a.Address)
			if a.Type != typ || err != nil {
				continue
			}
			ip = ip.Unmap()
			if directorAddrs[ip] {
				return nil, fmt.Sprintf("a director of this cluster runs on %s, at %s: remove it from the cluster (thelb admin remove), or label the node %s and taint it", name, ip, labelDirector)
			}
			addrs = append(addrs, ip)
		}
	}
	if len(addrs) == 0 {
		return nil, name + " has no InternalIP or ExternalIP address"
	}
	return addrs, ""
}

// kubeExcludedPods says which nodes run ready pods of a Service with policy
// Local - pods that get no traffic from outside, because theLB leaves their
// node out - and why each is left out; "" when none does. Nodes that are not
// Ready are left out of it: their pods stop being ready soon.
func kubeExcludedPods(svc *kube.Service, nodes map[string]*kube.Node, slices map[string]*kube.EndpointSlice, directorAddrs map[netip.Addr]bool) string {
	if svc.Spec.ExternalTrafficPolicy != "Local" {
		return "" // with Cluster, the other nodes pass traffic to them
	}
	pods := map[string]int{}
	for _, fam := range families(svc) {
		for node, n := range localEndpoints(slices, svc, fam) {
			pods[node] += n
		}
	}
	var says []string
	for _, name := range sortedNames(pods) {
		kn := nodes[name]
		if kn == nil || pods[name] == 0 || !kn.Ready() {
			continue
		}
		if _, why := nodeExclusion(kn, directorAddrs); why != "" {
			says = append(says, fmt.Sprintf("%d on %s - %s", pods[name], name, why))
		}
	}
	if len(says) == 0 {
		return ""
	}
	return "ready pods of this Service on nodes theLB sends no traffic to, so they get none from outside: " + strings.Join(says, "; ")
}

// localEndpoints counts each node's ready endpoints of a Service, in one
// address family.
func localEndpoints(slices map[string]*kube.EndpointSlice, svc *kube.Service, fam string) map[string]int {
	out := map[string]int{}
	for _, es := range slices {
		if es.Meta.Namespace != svc.Meta.Namespace || es.ServiceName() != svc.Meta.Name || es.AddressType != fam {
			continue
		}
		for _, ep := range es.Endpoints {
			if ep.IsReady() && ep.NodeName != nil {
				out[*ep.NodeName]++
			}
		}
	}
	return out
}

// kubeLive is a node serving one port of a Kubernetes Service, as the leader
// programs it.
type kubeLive struct {
	service string // namespace/name
	vip     netip.Addr
	proto   string
	port    int
	node    string
	addr    netip.Addr
	weight  int
	check   string // its check's key
}

// kubeLiveName names one port of a Kubernetes Service on one VIP, where
// status takes the leader's view of it.
func kubeLiveName(service string, vip netip.Addr, proto string, port int) string {
	return fmt.Sprintf("%s %s %s/%d", service, vip, proto, port)
}

// kubePlan adds theLB's Kubernetes Services to the plan: an IPVS service for
// each port on each VIP, with the nodes that serve it.
func (p *plane) kubePlan(ctx context.Context, plan *Plan, vips *[]PlanVIP, seenVIP, serving map[netip.Addr]bool, ifaceOf func(cluster.Pool) string,
	pools map[string]cluster.Pool, allocs []cluster.KubeService, nodes map[string]cluster.Node, checkers []string, wantProbers map[string]bool, given map[string]map[string]api.Probe) {
	s := p.s
	k := s.n.kube
	live := map[string]kubeLive{}
	p.mu.Lock()
	before := p.kube
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.kube = live
		p.mu.Unlock()
	}()
	if k == nil || len(allocs) == 0 {
		return
	}
	services := k.services.Items()
	directorAddrs := map[netip.Addr]bool{}
	for _, n := range nodes {
		if a, err := netip.ParseAddr(n.Address); err == nil && n.Role == "director" {
			directorAddrs[a] = true
		}
	}
	targets := kubeTargets(k.nodes.Items(), directorAddrs)
	allSlices := k.slices.Items()
	reach := map[netip.Addr]bool{} // node addresses this director reaches directly
	reaches := func(a netip.Addr) bool {
		ok, seen := reach[a]
		if !seen {
			_, err := s.n.local(a, "")
			ok = err == nil
			reach[a] = ok
		}
		return ok
	}
	sort.Slice(allocs, func(i, j int) bool { return allocs[i].Key < allocs[j].Key })
	for _, a := range allocs {
		svc := services[a.Key]
		if svc == nil || svc.Meta.UID != a.UID || !eligible(svc) {
			continue
		}
		local := svc.Spec.ExternalTrafficPolicy == "Local" && svc.Spec.HealthCheckNodePort > 0
		for _, x := range a.VIPs {
			vip, err := netip.ParseAddr(x.VIP)
			if err != nil {
				continue
			}
			if !seenVIP[vip] {
				ifname := ifaceOf(pools[x.Pool])
				if ifname == "" {
					continue
				}
				seenVIP[vip] = true
				*vips = append(*vips, PlanVIP{Addr: vip, Interface: ifname})
			}
			fam := familyOf(vip)
			counts := localEndpoints(allSlices, svc, fam)
			for _, sp := range svc.Spec.Ports {
				proto := protoName(sp.Protocol)
				pnum, err := ipvs.ProtoNumber(proto)
				if err != nil {
					continue
				}
				if why, lacks := s.n.lacks["sctp"]; lacks && proto == "sctp" {
					s.log.Debug("an SCTP port of a Kubernetes Service skipped", "service", a.Key, "reason", why)
					continue
				}
				ps := PlanService{Service: ipvs.Service{Addr: vip, Protocol: pnum, Port: uint16(sp.Port), Scheduler: "wrr"}}
				for _, t := range targets {
					var addr netip.Addr
					for _, c := range t.addrs {
						if familyOf(c) == fam && reaches(c) {
							addr = c
							break
						}
					}
					if !addr.IsValid() {
						continue
					}
					spec, target := "none", netip.AddrPortFrom(addr, 0)
					switch {
					case local:
						spec, target = "http:/", netip.AddrPortFrom(addr, uint16(svc.Spec.HealthCheckNodePort))
					case proto == "tcp" && sp.NodePort > 0:
						spec, target = "tcp", netip.AddrPortFrom(addr, uint16(sp.NodePort))
					}
					key := spec + " " + target.String()
					if spec == "none" {
						key = "none " + t.name
					}
					wantProbers[key] = true
					healthy := !local || counts[t.name] > 0
					ok := true
					if spec != "none" {
						ok = p.check(ctx, key, spec, target, healthy, checkers, wantProbers, given)
					}
					w := 0
					if ok {
						w = 1
						if local {
							w = max(counts[t.name], 1)
						}
					}
					if w > 0 {
						serving[vip] = true
					}
					ps.Dests = append(ps.Dests, ipvs.Dest{Addr: addr, Port: uint16(sp.Port), Weight: uint32(w)})
					name := kubeLiveName(a.Key, vip, proto, sp.Port)
					l := kubeLive{service: a.Key, vip: vip, proto: proto, port: sp.Port, node: t.name, addr: addr, weight: w, check: key}
					live[name+"/"+t.name] = l
					if old, seen := before[name+"/"+t.name]; !seen || (old.weight > 0) != (w > 0) {
						at := netip.AddrPortFrom(addr, uint16(sp.Port))
						switch {
						case w > 0:
							s.log.OK("node serving", "service", a.Key, "vip", vip, "port", proto+"/"+fmt.Sprint(sp.Port), "node", t.name, "at", at, "weight", w)
						case seen || !local || counts[t.name] > 0:
							why := "its health port answers no 200: no ready pod of the Service there"
							if !local {
								why = "its node port does not answer"
							}
							s.log.Warn("node not serving: weight 0", "service", a.Key, "vip", vip, "port", proto+"/"+fmt.Sprint(sp.Port), "node", t.name, "at", at, "reason", why)
						}
					}
				}
				plan.Services = append(plan.Services, ps)
			}
		}
	}
}
