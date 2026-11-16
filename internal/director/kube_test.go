package director

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/ipvs"
	"github.com/dmartsapp/thelb/internal/kube"
	"github.com/dmartsapp/thelb/internal/localsock"
)

// recPlane is a data plane that keeps the last plan applied.
type recPlane struct {
	mu   sync.Mutex
	last Plan
}

func (f *recPlane) Apply(p Plan) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = p
	return nil
}
func (f *recPlane) plan() Plan                                        { f.mu.Lock(); defer f.mu.Unlock(); return f.last }
func (f *recPlane) Stats() map[string]ipvs.Stats                      { return nil }
func (f *recPlane) Clear()                                            {}
func (f *recPlane) Probe(context.Context, netip.Addr) (string, error) { return "", nil }
func (f *recPlane) ProbeAll(context.Context, []PlanVIP) (map[netip.Addr]string, error) {
	return nil, nil
}
func (f *recPlane) Leftovers(Plan) error   { return nil }
func (f *recPlane) Sync(SyncState) error   { return nil }
func (f *recPlane) Conns() ([]Conn, error) { return nil, nil }

// dest is the weight of a destination of the plan's service, or -1.
func (f *recPlane) dest(vip string, port int, addr string) int {
	for _, s := range f.plan().Services {
		if s.Addr.String() == vip && int(s.Port) == port {
			for _, d := range s.Dests {
				if d.Addr.String() == addr {
					return int(d.Weight)
				}
			}
		}
	}
	return -1
}

// fakeKube is a Kubernetes API server: lists from what it holds, watches that
// send what a test pushes, and a record of what was written.
type fakeKube struct {
	mu       sync.Mutex
	srv      *httptest.Server
	objects  map[string][]any // by list path
	watches  map[string]chan string
	patches  []string
	events   []string
	disallow map[string]bool // permissions refused: verb resource
}

func newFakeKube(t *testing.T) *fakeKube {
	f := &fakeKube{objects: map[string][]any{}, watches: map[string]chan string{}, disallow: map[string]bool{}}
	for _, p := range []string{"/api/v1/services", "/api/v1/nodes", "/apis/discovery.k8s.io/v1/endpointslices"} {
		f.watches[p] = make(chan string, 10)
	}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeKube) client() *kube.Client {
	u, _ := url.Parse(f.srv.URL)
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	return kube.New(&kube.Config{Server: u, RootCAs: pool, Token: "t", Source: "the test"})
}

func (f *fakeKube) set(path string, objs ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[path] = objs
}

func (f *fakeKube) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/version":
		_, _ = io.WriteString(w, `{"major":"1","minor":"36","gitVersion":"v1.36.4"}`)
	case strings.HasSuffix(r.URL.Path, "/selfsubjectaccessreviews"):
		var in struct {
			Spec struct {
				ResourceAttributes map[string]string `json:"resourceAttributes"`
			} `json:"spec"`
		}
		_ = json.Unmarshal(b, &in)
		a := in.Spec.ResourceAttributes
		f.mu.Lock()
		no := f.disallow[a["verb"]+" "+a["resource"]]
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"status":{"allowed":%v}}`, !no)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/status"):
		f.mu.Lock()
		f.patches = append(f.patches, r.URL.Path+" "+string(b))
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events"):
		var ev map[string]any
		_ = json.Unmarshal(b, &ev)
		f.mu.Lock()
		f.events = append(f.events, fmt.Sprint(ev["reason"], " ", ev["note"]))
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	case r.URL.Query().Get("watch") == "1":
		f.mu.Lock()
		ch := f.watches[r.URL.Path]
		f.mu.Unlock()
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-ch:
				_, _ = io.WriteString(w, ev+"\n")
				w.(http.Flusher).Flush()
			}
		}
	default:
		f.mu.Lock()
		objs, ok := f.objects[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"kind":"Status","code":404}`)
			return
		}
		out, _ := json.Marshal(map[string]any{"metadata": map[string]string{"resourceVersion": "1"}, "items": objs})
		_, _ = w.Write(out)
	}
}

func (f *fakeKube) saw(what string, in func(*fakeKube) []string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range in(f) {
		if strings.Contains(x, what) {
			return true
		}
	}
	return false
}

func kubeNode(name, addr string, ready bool, labels map[string]string) map[string]any {
	st := "True"
	if !ready {
		st = "False"
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "labels": labels},
		"status": map[string]any{
			"addresses":  []map[string]string{{"type": "InternalIP", "address": addr}},
			"conditions": []map[string]string{{"type": "Ready", "status": st}},
		},
	}
}

func webService(hcPort int, annotations map[string]string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "web", "namespace": "default", "uid": "u-web", "annotations": annotations},
		"spec": map[string]any{
			"type": "LoadBalancer", "loadBalancerClass": "dmarts.app/thelb", "externalTrafficPolicy": "Local",
			"healthCheckNodePort": hcPort, "ports": []map[string]any{{"name": "http", "protocol": "TCP", "port": 80, "nodePort": 31000}},
		},
	}
}

func TestDirectorServesKubernetesServices(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"localEndpoints":2}`)
	}))
	defer health.Close()
	hcPort := netip.MustParseAddrPort(strings.TrimPrefix(health.URL, "http://")).Port()

	f := newFakeKube(t)
	f.set("/api/v1/nodes",
		kubeNode("n1", "127.0.0.1", true, nil),
		kubeNode("n2", "10.255.255.1", true, nil), // its health port never answers
		kubeNode("n3", "127.0.0.1", true, map[string]string{"node.kubernetes.io/exclude-from-external-load-balancers": "true"}),
		kubeNode("n4", "127.0.0.1", false, nil),
	)
	other := webService(1, nil)
	other["metadata"] = map[string]any{"name": "theirs", "namespace": "default", "uid": "u-t"}
	other["spec"].(map[string]any)["loadBalancerClass"] = "example.com/other"
	f.set("/api/v1/services", webService(int(hcPort), nil), other)
	f.set("/apis/discovery.k8s.io/v1/endpointslices", map[string]any{
		"metadata":    map[string]any{"name": "web-abc", "namespace": "default", "labels": map[string]string{"kubernetes.io/service-name": "web"}},
		"addressType": "IPv4",
		"endpoints": []map[string]any{
			{"addresses": []string{"10.1.0.5"}, "conditions": map[string]bool{"ready": true}, "nodeName": "n1"},
			{"addresses": []string{"10.1.0.6"}, "nodeName": "n1"},
			{"addresses": []string{"10.1.0.7"}, "conditions": map[string]bool{"ready": false}, "nodeName": "n2"},
			{"addresses": []string{"10.1.0.8"}, "conditions": map[string]bool{"ready": true}, "nodeName": "n3"},
		},
	})

	w := single(t)
	plane := &recPlane{}
	w.plane = plane
	w.cfg.Kubeconfig = "the test's"
	w.kube = f.client()
	w.start(t)
	eventually(t, "it leads", func() bool { return strings.Contains(w.logs.String(), "OK control VIP taken") })
	if !strings.Contains(w.logs.String(), "OK connected to Kubernetes: serving Services of class dmarts.app/thelb api="+f.srv.URL+" version=v1.36.4 credential=\"the test\" permissions=\"all 13\"") {
		t.Fatalf("not connected to Kubernetes:\n%s", w.logs)
	}
	eventually(t, "the cluster read", func() bool {
		return strings.Contains(w.logs.String(), "OK watching Kubernetes: the cluster read services=1 nodes=4 ready=3")
	})
	resp, err := localsock.Call(w.run, localsock.Request{Op: localsock.OpChange, Change: &api.Change{Op: "pool.add", Pool: "k8s", VIP: "10.0.0.50/24", Count: 4, Default: true}})
	if err != nil || resp.Error != "" {
		t.Fatalf("pool: %v %s", err, resp.Error)
	}
	eventually(t, "the Service's status", func() bool {
		return f.saw(`/api/v1/namespaces/default/services/web/status {"status":{"loadBalancer":{"ingress":[{"ip":"10.0.0.50","ipMode":"VIP"}]}}}`, func(f *fakeKube) []string { return f.patches })
	})
	if f.saw("theirs", func(f *fakeKube) []string { return f.patches }) {
		t.Error("another provider's Service was given a VIP")
	}
	eventually(t, "the event", func() bool {
		return f.saw("IPAllocated theLB gave it 10.0.0.50", func(f *fakeKube) []string { return f.events })
	})
	eventually(t, "a warning: n3's pod gets no traffic, and why", func() bool {
		return f.saw("NodesExcluded ready pods of this Service on nodes theLB sends no traffic to, so they get none from outside: 1 on n3 - n3 has the label node.kubernetes.io/exclude-from-external-load-balancers, which kubeadm puts on control-plane nodes; to send it traffic: kubectl label node n3 node.kubernetes.io/exclude-from-external-load-balancers-", func(f *fakeKube) []string { return f.events })
	})
	if !strings.Contains(w.logs.String(), "WARN Kubernetes Service: ready pods of this Service on nodes theLB sends no traffic to") {
		t.Errorf("the warning is not in the log:\n%s", w.logs)
	}
	eventually(t, "n1 serving with its two ready pods, n2 not", func() bool {
		return plane.dest("10.0.0.50", 80, "127.0.0.1") == 2 && plane.dest("10.0.0.50", 80, "10.255.255.1") == 0
	})
	eventually(t, "the VIP held", func() bool {
		for _, v := range plane.plan().VIPs {
			if v.Addr.String() == "10.0.0.50" {
				return true
			}
		}
		return false
	})
	st := w.status(t)
	if st.Cluster.Kubernetes != "1 Services" {
		t.Errorf("cluster's kubernetes: %q", st.Cluster.Kubernetes)
	}
	found := false
	for _, s := range st.Services {
		if s.Service == "default/web" && s.VIP == "10.0.0.50" && s.Port == 80 && s.Backends == 2 && s.BackendsUp == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("services: %+v", st.Services)
	}
	time.Sleep(time.Second) // five more reconciles: each warning said once, not again and again
	f.mu.Lock()
	n := 0
	for _, e := range f.events {
		if strings.HasPrefix(e, "NodesExcluded ") {
			n++
		}
	}
	f.mu.Unlock()
	if n != 1 {
		t.Errorf("NodesExcluded said %d times", n)
	}
	var vipRow bool
	for _, v := range st.VIPs {
		vipRow = vipRow || (v.VIP == "10.0.0.50" && v.Purpose == "kubernetes" && v.HeldBy == "lb-1")
	}
	if !vipRow {
		t.Errorf("vips: %+v", st.VIPs)
	}

	// A plain service cannot take the Kubernetes Service's VIP.
	resp, _ = localsock.Call(w.run, localsock.Request{Op: localsock.OpChange, Change: &api.Change{Op: "service.add", Names: []string{"plain"}, Proto: "tcp", Port: 81, VIP: "10.0.0.50"}})
	if !strings.Contains(resp.Error, "Kubernetes Service default/web") {
		t.Errorf("a plain service on the Kubernetes VIP: %q", resp.Error)
	}

	// The watch lost - the API server fails - and back: each said once.
	f.watches["/api/v1/services"] <- `{"type":"ERROR","object":{"kind":"Status","code":500,"message":"etcd is down"}}`
	eventually(t, "the watch lost, then back", func() bool {
		l := w.logs.String()
		return strings.Contains(l, `WARN Kubernetes watch lost: retrying - meanwhile the leader serves what it last read reason="services: the API server answered 500: etcd is down"`) &&
			strings.Contains(l, "OK Kubernetes watch back")
	})
	if n := strings.Count(w.logs.String(), "Kubernetes watch lost"); n != 1 {
		t.Errorf("the lost watch said %d times", n)
	}

	// The Service deleted: its VIP goes back, and its IPVS service.
	f.watches["/api/v1/services"] <- `{"type":"DELETED","object":{"metadata":{"name":"web","namespace":"default","uid":"u-web","resourceVersion":"2"}}}`
	eventually(t, "the VIP taken back", func() bool {
		return strings.Contains(w.logs.String(), "OK Kubernetes Service gone: its VIPs taken back service=default/web") && plane.dest("10.0.0.50", 80, "127.0.0.1") == -1
	})
}

func TestADirectorOnAnUnlabelledNodeRefusesToStart(t *testing.T) {
	f := newFakeKube(t)
	f.set("/api/v1/nodes", kubeNode("cp1", "10.0.0.11", true, nil))
	w := single(t)
	w.cfg.Kubeconfig = "the test's"
	w.kube = f.client()
	w.launch(t)
	err := <-w.errc
	w.errc <- err
	if err == nil || !strings.Contains(err.Error(), "runs on the Kubernetes node cp1, which is not labelled as a director") {
		t.Fatalf("%v", err)
	}
}

func TestADirectorWithoutThePermissionsSaysWhich(t *testing.T) {
	f := newFakeKube(t)
	f.set("/api/v1/nodes")
	f.disallow["patch services"] = true
	f.disallow["watch nodes"] = true
	w := single(t)
	w.cfg.Kubeconfig = "the test's"
	w.kube = f.client()
	w.launch(t)
	err := <-w.errc
	w.errc <- err
	if err == nil || !strings.Contains(err.Error(), "lacks permissions theLB needs: patch services/status, watch nodes") {
		t.Fatalf("%v", err)
	}
}

// After every director stopped, the new cluster knows no allocation: a Service
// takes back the VIP its status names, and a new one does not take it first.
func TestAServiceKeepsItsVIPAfterEveryDirectorStopped(t *testing.T) {
	f := newFakeKube(t)
	f.set("/api/v1/nodes", kubeNode("n1", "127.0.0.1", true, nil))
	web := webService(30100, nil)
	web["status"] = map[string]any{"loadBalancer": map[string]any{"ingress": []map[string]string{{"ip": "10.0.0.50", "ipMode": "VIP"}}}}
	other := webService(30101, nil)
	other["metadata"] = map[string]any{"name": "api", "namespace": "default", "uid": "u-api"} // sorts first: reached first
	f.set("/api/v1/services", web, other)
	f.set("/apis/discovery.k8s.io/v1/endpointslices")

	w := single(t)
	w.plane = &recPlane{}
	w.cfg.Kubeconfig = "the test's"
	w.kube = f.client()
	w.start(t)
	eventually(t, "it leads", func() bool { return strings.Contains(w.logs.String(), "OK control VIP taken") })
	resp, err := localsock.Call(w.run, localsock.Request{Op: localsock.OpChange, Change: &api.Change{Op: "pool.add", Pool: "k8s", VIP: "10.0.0.50/24", Count: 4, Default: true}})
	if err != nil || resp.Error != "" {
		t.Fatalf("pool: %v %s", err, resp.Error)
	}
	eventually(t, "web keeps 10.0.0.50, the lowest; api passes over it to 10.0.0.51", func() bool {
		return f.saw(`/api/v1/namespaces/default/services/api/status {"status":{"loadBalancer":{"ingress":[{"ip":"10.0.0.51","ipMode":"VIP"}]}}}`, func(f *fakeKube) []string { return f.patches }) &&
			strings.Contains(w.logs.String(), "OK Kubernetes Service given its VIPs service=default/web vips=10.0.0.50")
	})
	if f.saw("services/web/status", func(f *fakeKube) []string { return f.patches }) {
		t.Error("web's status written again, though it names its VIP already")
	}
}
