package check

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestParse(t *testing.T) {
	for _, c := range []struct{ spec, proto, want string }{
		{"", "tcp", "tcp"}, {"", "udp", "none"}, {"", "sctp", "none"},
		{"http:/healthz", "tcp", "http:/healthz"}, {"none", "udp", "none"},
	} {
		if got, err := Parse(c.spec, c.proto); err != nil || got != c.want {
			t.Errorf("Parse(%q, %q) = %q, %v", c.spec, c.proto, got, err)
		}
	}
	for _, c := range [][2]string{{"tcp", "udp"}, {"http:/x", "udp"}, {"http:healthz", "tcp"}, {"icmp", "tcp"}} {
		if _, err := Parse(c[0], c[1]); err == nil {
			t.Errorf("Parse(%q, %q) took it", c[0], c[1])
		}
	}
	if Describe("http:/healthz") != "http /healthz" || Describe("tcp") != "tcp" {
		t.Error("Describe")
	}
}

func TestProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	ap := netip.MustParseAddrPort(srv.Listener.Addr().String())
	ctx := context.Background()
	for spec, ok := range map[string]bool{"tcp": true, "none": true, "http:/healthz": true, "http:/down": false} {
		if err := Probe(ctx, spec, ap, Timeout); (err == nil) != ok {
			t.Errorf("%s: %v", spec, err)
		}
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := netip.MustParseAddrPort(ln.Addr().String())
	_ = ln.Close()
	if err := Probe(ctx, "tcp", closed, Timeout); err == nil {
		t.Error("a closed port passed")
	}
}

func TestRiseAndFall(t *testing.T) {
	var s State
	fail := errors.New("refused")
	if s.Observe(nil) || s.Healthy {
		t.Error("healthy after one success")
	}
	if !s.Observe(nil) || !s.Healthy {
		t.Error("not healthy after two")
	}
	first, second := s.Observe(fail), s.Observe(fail)
	if first || second || !s.Healthy {
		t.Error("unhealthy before three failures")
	}
	if s.Observe(nil); !s.Healthy {
		t.Error("a success between failures")
	}
	s.Observe(fail)
	s.Observe(fail)
	if !s.Observe(fail) || s.Healthy || s.Word() != "fail" {
		t.Error("still healthy after three failures in a row")
	}
}
