package director

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dmartsapp/thelb/internal/cluster"
)

// Commands is a shell script of the `thelb admin` commands that would create
// the cluster's pools, services and assignments again, as they are in st -
// for the user to keep, and to run on a cluster created again after every
// director stopped. theLB never reads it: it is printed, not configuration.
// from is the director it was taken on, at when.
func Commands(st *cluster.State, from string, at time.Time) string {
	var b strings.Builder
	name := "-"
	if st.Cluster != nil {
		name = st.Cluster.Name
	}
	fmt.Fprintf(&b, `#!/bin/sh
# theLB: the commands that create the pools, services and assignments of the
# cluster %s again, as they were at %s, on %s.
#
# For a cluster created again after every director stopped: run them, as root,
# on one of its directors - once its nodes are approved, and the backends'
# offers are listed by "thelb status backends": an assignment needs its
# backend's offer. theLB prints this; it never reads it.
`, name, at.UTC().Format(time.RFC3339), from)

	var others []string
	for _, n := range sortedNames(st.Nodes) {
		if n != from {
			others = append(others, n)
		}
	}
	if len(others) > 0 {
		fmt.Fprintf(&b, "#\n# The nodes that were members, to approve - after comparing fingerprints - once\n# they ask:\n#   thelb admin approve --match '%s'\n", strings.Join(others, "|"))
	}

	if len(st.Pools) > 0 {
		b.WriteString("\n# Pools\n")
	}
	for _, n := range sortedNames(st.Pools) {
		p := st.Pools[n]
		fmt.Fprintf(&b, "thelb admin pool add %s --vip %s/%d --vippool-count %d", p.Name, p.First, p.Bits, p.Count)
		if p.Interface != "" {
			fmt.Fprintf(&b, " --interface %s", p.Interface)
		}
		if p.Default {
			b.WriteString(" --default")
		}
		b.WriteString("\n")
	}

	if len(st.Services) > 0 {
		b.WriteString("\n# Services, on the addresses they had\n")
	}
	for _, n := range sortedNames(st.Services) {
		v := st.Services[n]
		method := v.Method
		if method == "" {
			method = "direct"
		}
		fmt.Fprintf(&b, "thelb admin service add %s --pool %s --vip %s --proto %s --port %d --scheduler %s --method %s\n",
			v.Name, v.Pool, v.VIP, v.Proto, v.Port, v.Scheduler, method)
	}

	byService := map[string][]*cluster.Assignment{}
	for _, a := range st.Assignments {
		byService[a.Service] = append(byService[a.Service], a)
	}
	if len(byService) > 0 {
		b.WriteString("\n# Assignments\n")
	}
	for _, svc := range sortedNames(byService) {
		as := byService[svc]
		sort.Slice(as, func(i, j int) bool { return as[i].Node < as[j].Node })
		var offers, weights []string
		for _, a := range as {
			offers = append(offers, fmt.Sprintf("%s:%s/%d", a.Node, a.Proto, a.Port))
			if a.Weight != 1 {
				weights = append(weights, fmt.Sprintf("%s=%d", a.Node, a.Weight))
			}
		}
		fmt.Fprintf(&b, "thelb admin assign %s %s", svc, strings.Join(offers, " "))
		if len(weights) > 0 {
			fmt.Fprintf(&b, " --weight %s", strings.Join(weights, ","))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Generate prints what `thelb generate` asks of the running director.
func (n *node) Generate(what string) (string, error) {
	if what != "commands" {
		return "", fmt.Errorf("a director generates commands, not %q", what)
	}
	n.mu.Lock()
	s := n.sess
	n.mu.Unlock()
	if s == nil {
		return "", errors.New("this director is not a member of a cluster yet")
	}
	var text string
	s.fsm.Read(func(st *cluster.State) { text = Commands(st, n.cfg.Name, n.d.Now()) })
	return text, nil
}
