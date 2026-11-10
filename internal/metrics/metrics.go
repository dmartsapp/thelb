// Package metrics serves a node's state in Prometheus's text format, on an
// address the user names: what `thelb status` shows, as numbers. Every
// director says how it stands - leading or not, its term, the nodes it sees -
// and the leader alone says what the cluster serves - its VIPs, its backends'
// weights, checks and counters - so nothing is counted twice.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/status"
	"github.com/dmartsapp/thelb/internal/version"
)

// family is one metric: its name, help, type and samples.
type family struct {
	name, help, kind string
	samples          []sample
}

type sample struct {
	labels [][2]string
	value  float64
}

type set struct{ fams []*family }

func (s *set) add(name, kind, help string) *family {
	f := &family{name: name, help: help, kind: kind}
	s.fams = append(s.fams, f)
	return f
}

func (f *family) put(value float64, labels ...string) {
	var ls [][2]string
	for i := 0; i+1 < len(labels); i += 2 {
		ls = append(ls, [2]string{labels[i], labels[i+1]})
	}
	f.samples = append(f.samples, sample{ls, value})
}

func b(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

// text is the set in Prometheus's text format, version 0.0.4; a family without
// samples is left out.
func (s *set) text() string {
	var w strings.Builder
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	for _, f := range s.fams {
		if len(f.samples) == 0 {
			continue
		}
		fmt.Fprintf(&w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.kind)
		for _, sm := range f.samples {
			w.WriteString(f.name)
			if len(sm.labels) > 0 {
				parts := make([]string, len(sm.labels))
				for i, l := range sm.labels {
					parts[i] = l[0] + `="` + esc.Replace(l[1]) + `"`
				}
				w.WriteString("{" + strings.Join(parts, ",") + "}")
			}
			fmt.Fprintf(&w, " %g\n", sm.value)
		}
	}
	return w.String()
}

// Director is a director's metrics, from its status: self is its name, and
// takeovers how many times it stood alone.
func Director(st status.Status, self string, takeovers int64) string {
	var s set
	s.add("thelb_build_info", "gauge", "theLB's version and commit, as labels.").put(1, "role", "director", "version", version.Version, "commit", version.Commit)
	leads, member := false, false
	if c := st.Cluster; c != nil {
		leads = c.Leader == self
		if c.Term != nil {
			s.add("thelb_raft_term", "gauge", "The Raft term this director knows.").put(float64(*c.Term))
		}
		if c.Lease > 0 {
			s.add("thelb_vip_lease_seconds", "gauge", "How long the kernel keeps a VIP after the leader's last renewal.").put(float64(c.Lease))
		}
	}
	nodes := map[[2]string]int{}
	for _, n := range st.Nodes {
		role := n.Role
		if role == "leader" {
			role = "director"
		}
		nodes[[2]string{role, n.State}]++
		if n.Node == self && (n.State == "up" || n.Role == "leader" || n.State == "catching up") {
			member = true
		}
	}
	s.add("thelb_member", "gauge", "Whether this node is a member of a cluster.").put(b(member || leads))
	s.add("thelb_leader", "gauge", "Whether this director leads the cluster.").put(b(leads))
	nf := s.add("thelb_nodes", "gauge", "The cluster's nodes, by role and state, as this director sees them.")
	for _, k := range sortedKeys(nodes) {
		nf.put(float64(nodes[k]), "role", k[0], "state", k[1])
	}
	s.add("thelb_takeovers_total", "counter", "How many times this director stood alone, the other of two gone.").put(float64(takeovers))
	if !leads {
		return s.text()
	}
	vf := s.add("thelb_vip_held", "gauge", "The VIPs this leader holds: 1 for each.")
	for _, v := range st.VIPs {
		if v.HeldBy == self {
			vf.put(1, "vip", v.VIP, "purpose", v.Purpose, "interface", v.Interface)
		}
	}
	total := s.add("thelb_service_backends", "gauge", "The backends assigned to a service.")
	up := s.add("thelb_service_backends_up", "gauge", "The backends of a service with a weight above 0.")
	for _, v := range st.Services {
		total.put(float64(v.Backends), "service", v.Service, "vip", v.VIP)
		up.put(float64(v.BackendsUp), "service", v.Service, "vip", v.VIP)
	}
	weight := s.add("thelb_backend_weight", "gauge", "A backend's weight in a service, as IPVS has it: 0 while it does not serve.")
	check := s.add("thelb_backend_check_up", "gauge", "Whether a backend's check passes: its own, and the directors'.")
	conns := s.add("thelb_backend_connections_total", "counter", "Connections IPVS sent a backend of a service.")
	pkts := s.add("thelb_backend_packets_total", "counter", "Packets IPVS sent a backend of a service.")
	bytes := s.add("thelb_backend_bytes_total", "counter", "Bytes IPVS sent a backend of a service.")
	active := s.add("thelb_backend_connections_active", "gauge", "A backend's connections of a service established now, as IPVS tracks them.")
	inactive := s.add("thelb_backend_connections_inactive", "gauge", "A backend's other connections of a service IPVS still tracks: closing, or not yet established.")
	type sum struct{ conns, pkts, bytes, active float64 }
	traffic := map[[2]string]*sum{}
	var order [][2]string
	for _, x := range st.Backends {
		weight.put(float64(x.Weight), "service", x.Service, "backend", x.Backend)
		check.put(b(x.OwnCheck == "ok"), "service", x.Service, "backend", x.Backend, "check", "own")
		check.put(b(x.DirectorCheck == "ok"), "service", x.Service, "backend", x.Backend, "check", "director")
		conns.put(float64(x.Connections), "service", x.Service, "backend", x.Backend)
		pkts.put(float64(x.Packets), "service", x.Service, "backend", x.Backend)
		bytes.put(float64(x.Bytes), "service", x.Service, "backend", x.Backend)
		active.put(float64(x.ActiveConnections), "service", x.Service, "backend", x.Backend)
		inactive.put(float64(x.InactiveConns), "service", x.Service, "backend", x.Backend)
		k := [2]string{x.Service, x.VIP}
		if traffic[k] == nil {
			traffic[k] = &sum{}
			order = append(order, k)
		}
		t := traffic[k]
		t.conns, t.pkts, t.bytes, t.active = t.conns+float64(x.Connections), t.pkts+float64(x.Packets), t.bytes+float64(x.Bytes), t.active+float64(x.ActiveConnections)
	}
	// The traffic through the director, by service: what came in for its VIP -
	// direct routing and IP-in-IP send the replies past it.
	sc := s.add("thelb_service_connections_total", "counter", "Connections the director forwarded for a service.")
	sp := s.add("thelb_service_packets_total", "counter", "Packets the director forwarded for a service: what came in; the replies go past it.")
	sb := s.add("thelb_service_bytes_total", "counter", "Bytes the director forwarded for a service: what came in; the replies go past it.")
	sa := s.add("thelb_service_connections_active", "gauge", "A service's connections established now.")
	for _, k := range order {
		t := traffic[k]
		sc.put(t.conns, "service", k[0], "vip", k[1])
		sp.put(t.pkts, "service", k[0], "vip", k[1])
		sb.put(t.bytes, "service", k[0], "vip", k[1])
		sa.put(t.active, "service", k[0], "vip", k[1])
	}
	return s.text()
}

// Backend is a backend's metrics, from its status.
func Backend(st status.Status) string {
	var s set
	s.add("thelb_build_info", "gauge", "theLB's version and commit, as labels.").put(1, "role", "backend", "version", version.Version, "commit", version.Commit)
	m := st.Mine
	s.add("thelb_member", "gauge", "Whether this node is a member of a cluster.").put(b(m != nil && m.State == "a member of the cluster"))
	if m == nil {
		return s.text()
	}
	own := s.add("thelb_offer_check_up", "gauge", "Whether this backend's own check of an offer passes.")
	assigned := s.add("thelb_offer_assigned", "gauge", "Whether an offer is assigned to a service: 1, with the service as a label.")
	for _, o := range m.Offers {
		own.put(b(o.Own == "ok"), "offer", o.Offer, "port", o.Port)
		if o.Service != "" && o.Service != "-" {
			assigned.put(1, "offer", o.Offer, "service", o.Service)
		}
	}
	return s.text()
}

func sortedKeys(m map[[2]string]int) [][2]string {
	out := make([][2]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0]+"/"+out[i][1] < out[j][0]+"/"+out[j][1] })
	return out
}

// Serve answers GET /metrics on addr with what gather returns, until ctx ends.
func Serve(ctx context.Context, addr string, gather func() string, log *logx.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("--metrics %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(gather()))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.OK("metrics served", "address", ln.Addr().String(), "path", "/metrics")
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serving metrics failed", "error", err)
		}
	}()
	return nil
}
