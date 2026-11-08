//go:build linux

package director

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/api"
	"github.com/dmartsapp/thelb/internal/backend"
	"github.com/dmartsapp/thelb/internal/cluster"
	"github.com/dmartsapp/thelb/internal/fingerprint"
	"github.com/dmartsapp/thelb/internal/hostnet"
	"github.com/dmartsapp/thelb/internal/ipvs"
	"github.com/dmartsapp/thelb/internal/localsock"
	"github.com/dmartsapp/thelb/internal/logx"
)

// fakePlane records the plan a leader applies.
type fakePlane struct {
	mu      sync.Mutex
	plan    Plan
	cleared int
}

func (f *fakePlane) Apply(p Plan) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plan = p
	return nil
}
func (f *fakePlane) Stats() map[string]ipvs.Stats { return nil }
func (f *fakePlane) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plan = Plan{}
	f.cleared++
}
func (f *fakePlane) Probe(context.Context, netip.Addr) (string, error) { return "", nil }
func (f *fakePlane) Leftovers(Plan) error                              { return nil }

// weight is the weight the plan gives a backend in a service, or -1.
func (f *fakePlane) weight(vip string, port int, backend netip.AddrPort) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.plan.Services {
		if s.Addr.String() == vip && int(s.Port) == port {
			for _, d := range s.Dests {
				if netip.AddrPortFrom(d.Addr, d.Port) == backend {
					return int(d.Weight)
				}
			}
		}
	}
	return -1
}

// fakeKernel records the VIPs a backend holds.
type fakeKernel struct {
	mu       sync.Mutex
	vips     []netip.Addr
	restored bool
}

func (k *fakeKernel) Prepare() error { return nil }
func (k *fakeKernel) Hold(v []netip.Addr) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.vips = append([]netip.Addr(nil), v...)
	return nil
}
func (k *fakeKernel) Restore() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.vips, k.restored = nil, true
}
func (k *fakeKernel) holds(a string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, v := range k.vips {
		if v.String() == a {
			return true
		}
	}
	return false
}

func change(t *testing.T, w *world, c api.Change) (api.Result, error) {
	t.Helper()
	resp, err := localsock.Call(w.run, localsock.Request{Op: localsock.OpChange, Change: &c})
	if err != nil {
		return api.Result{}, err
	}
	return *resp.Result, nil
}

func TestABackendServesAService(t *testing.T) {
	l := newLab(t)
	d1 := l.director(t, 1)
	plane := &fakePlane{}
	d1.plane = plane
	d1.start(t)

	// The backend's application: HTTP on its own address.
	app := "127.0.0.21"
	ln, err := net.Listen("tcp", app+":0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	healthy := make(chan bool, 1)
	healthy <- true
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := <-healthy
		healthy <- ok
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// The backend, with the backends' secret copied from the director.
	base, err := os.MkdirTemp("/tmp", "thelb-be-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	b, _ := os.ReadFile(d1.cfg.BackendSecretFile)
	_ = os.WriteFile(filepath.Join(base, "backend.secret"), b, 0o600)
	kernel := &fakeKernel{}
	logs := &syncBuffer{}
	ready := make(chan struct{})
	id, _ := fingerprint.ParseMachineID("4c2f3a0e9b7d4e1fa6c58d2b91e0f799")
	l.macs.Store(app, "52:54:00:00:00:21")
	cfg := backend.Config{
		Join:       netip.AddrPortFrom(d1.cfg.ControlVIP, d1.cfg.ControlPort),
		SecretFile: filepath.Join(base, "backend.secret"),
		Name:       "web-1",
		Offers:     []cluster.Offer{{Proto: "tcp", Port: port, Check: "http:/healthz"}},
	}
	run := filepath.Join(base, "run")
	bctx, bstop := context.WithCancel(context.Background())
	berr := make(chan error, 1)
	go func() {
		berr <- backend.Run(bctx, cfg, backend.Deps{
			RunDir: run, Log: logx.New(logs, "thelb"), Now: time.Now,
			Host: func(mac string) (fingerprint.Inputs, error) {
				return fingerprint.Inputs{MachineID: id, MAC: mac, Hostname: "web-1"}, nil
			},
			Local: func(netip.Addr, string) (hostnet.Address, error) {
				return hostnet.Address{Interface: "eth1", MAC: "52:54:00:00:00:21", Prefix: netip.MustParsePrefix(app + "/8")}, nil
			},
			Kernel:  func() (backend.Kernel, error) { return kernel, nil },
			PeerUID: func(*net.UnixConn) (int, error) { return 0, nil },
			Ready:   func() { close(ready) },
		})
	}()
	t.Cleanup(func() {
		bstop()
		if err := <-berr; err != nil {
			t.Logf("the backend ended with %v", err)
		}
	})

	eventually(t, "web-1 pending", func() bool { return pending(d1.status(t), "web-1") != nil })
	mine, _ := localsock.Call(run, localsock.Request{Op: localsock.OpStatus})
	if m := mine.Status.Mine; m == nil || m.State != "waiting for approval" || m.NodeFingerprint != pending(d1.status(t), "web-1").NodeFingerprint {
		t.Fatalf("the pending backend's own status: %+v", mine.Status.Mine)
	}
	if _, err := change(t, d1, api.Change{Op: "approve", Names: []string{"web-1"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case err := <-berr:
		t.Fatalf("the backend stopped: %v\n%s", err, logs)
	case <-time.After(20 * time.Second):
		t.Fatalf("the backend did not become a member\n%s\n%s", logs, d1.logs)
	}
	eventually(t, "the offer recorded", func() bool { return len(d1.status(t).Offers) == 1 })

	// A pool, a service, and the offer assigned to it.
	if _, err := change(t, d1, api.Change{Op: "pool.add", Pool: "lab", VIP: "127.0.0.50/8", Count: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := change(t, d1, api.Change{Op: "pool.add", Pool: "overlap", VIP: "127.0.0.52/8", Count: 5}); err == nil || !strings.Contains(err.Error(), "overlaps the pool lab") {
		t.Errorf("an overlapping pool: %v", err)
	}
	r, err := change(t, d1, api.Change{Op: "service.add", Names: []string{"web"}, Pool: "lab", Proto: "tcp", Port: port})
	if err != nil || !strings.Contains(r.Message, "127.0.0.50 tcp/") {
		t.Fatalf("service add: %v %+v", err, r)
	}
	if _, err := change(t, d1, api.Change{Op: "assign", Names: []string{"web"}, Offers: []string{fmt.Sprintf("web-1:tcp/%d", port+1)}}); err == nil {
		t.Error("an offer the backend does not make was assigned")
	}
	if _, err := change(t, d1, api.Change{Op: "assign", Names: []string{"web"}, Offers: []string{fmt.Sprintf("web-1:tcp/%d", port)}, Weights: map[string]int{"web-1": 3}}); err != nil {
		t.Fatal(err)
	}
	dest := netip.AddrPortFrom(netip.MustParseAddr(app), uint16(port))
	eventually(t, "the backend to hold the VIP", func() bool { return kernel.holds("127.0.0.50") })
	eventually(t, "the leader to send it traffic, weight 3", func() bool { return plane.weight("127.0.0.50", port, dest) == 3 })
	plane.mu.Lock()
	vips, pins := plane.plan.VIPs, plane.plan.Pins
	plane.mu.Unlock()
	if len(vips) != 1 || vips[0].Addr.String() != "127.0.0.50" || len(pins) != 1 || pins[0].MAC != "52:54:00:00:00:21" {
		t.Errorf("the plan's VIPs %+v and pins %+v", vips, pins)
	}
	st := d1.status(t)
	if len(st.Backends) != 1 || st.Backends[0].OwnCheck != "ok" || st.Backends[0].DirectorCheck != "ok" || st.Backends[0].Weight != 3 || st.Services[0].BackendsUp != 1 {
		t.Errorf("status backends %+v services %+v", st.Backends, st.Services)
	}
	if text, _ := st.Text(time.Now(), "vips"); !strings.Contains(text, " 127.0.0.50 | service | lab ") {
		t.Errorf("status vips:\n%s", text)
	}
	mine, _ = localsock.Call(run, localsock.Request{Op: localsock.OpStatus})
	if m := mine.Status.Mine; m == nil || len(m.Offers) != 1 || m.Offers[0].Service != "web" || strings.Contains(fmt.Sprint(m), "127.0.0.50") {
		t.Errorf("the backend's own status: %+v", mine.Status.Mine)
	}

	// The application fails: both checks see it, and the weight goes to 0.
	<-healthy
	healthy <- false
	eventually(t, "weight 0 for a failing backend", func() bool { return plane.weight("127.0.0.50", port, dest) == 0 })
	if !strings.Contains(d1.logs.String(), "WARN backend not serving: weight 0 service=web node=web-1") {
		t.Errorf("the leader's log:\n%s", d1.logs)
	}
	<-healthy
	healthy <- true
	eventually(t, "the weight back", func() bool { return plane.weight("127.0.0.50", port, dest) == 3 })

	// Removed, the backend stops - and gives its VIPs up.
	if _, err := change(t, d1, api.Change{Op: "remove", Names: []string{"web-1"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-berr:
		berr <- err
		if err == nil || !strings.Contains(err.Error(), "removed from the cluster") {
			t.Errorf("the removed backend ended with %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the removed backend still runs\n%s", logs)
	}
	if !kernel.restored {
		t.Error("the removed backend left its kernel state")
	}
	eventually(t, "the service without backends", func() bool { return plane.weight("127.0.0.50", port, dest) == -1 })
}
