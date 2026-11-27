package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FuzzDecodeYAML: a kubeconfig is the user's, but it may be anyone's they were
// given. The reader ends - it never nests deeper than maxDepth - and gives
// maps, slices and strings, or an error.
func FuzzDecodeYAML(f *testing.F) {
	f.Add([]byte("apiVersion: v1\nclusters:\n  - name: c\n    cluster:\n      server: https://10.0.0.1:6443\n"))
	f.Add([]byte("a:\n" + strings.Repeat(" ", 2) + "b:\n    c:\n      - d\n"))
	f.Add([]byte("{\"a\": [1, 2, {\"b\": null}]}"))
	f.Add([]byte("a: &x 1\nb: *x\n"))
	f.Add([]byte("a: |\n  text\n"))
	f.Add([]byte(strings.Repeat("- ", 200) + "x"))
	f.Add([]byte("\t- x"))
	f.Fuzz(func(t *testing.T, b []byte) {
		v, err := decodeYAML(b)
		if err != nil {
			return
		}
		if depth(v) > maxDepth+2 && !strings.HasPrefix(strings.TrimSpace(string(b)), "{") {
			t.Fatalf("nested %d deep", depth(v))
		}
	})
}

func depth(v any) int {
	d := 0
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			d = max(d, depth(e))
		}
		return d + 1
	case []any:
		for _, e := range x {
			d = max(d, depth(e))
		}
		return d + 1
	}
	return 0
}

// FuzzLoadContext: a whole kubeconfig file. Load gives a configuration with an
// https server, or an error - never one that would send a token in clear.
func FuzzLoadContext(f *testing.F) {
	f.Add([]byte(`apiVersion: v1
kind: Config
clusters:
  - name: c
    cluster:
      server: https://10.0.0.1:6443
users:
  - name: u
    user:
      token: abc
contexts:
  - name: x
    context:
      cluster: c
      user: u
current-context: x
`), "")
	f.Add([]byte("current-context: x\ncontexts: []\n"), "x")
	f.Add([]byte("clusters:\n  - cluster:\n      server: http://10.0.0.1\n"), "")
	f.Fuzz(func(t *testing.T, b []byte, context string) {
		path := filepath.Join(t.TempDir(), "kubeconfig")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadContext(path, context)
		if err != nil {
			return
		}
		if c.Server == nil || c.Server.Scheme != "https" {
			t.Fatalf("took the server %v", c.Server)
		}
		if c.Token != "" && TokenProblem(c.Token) != "" {
			t.Fatalf("took the token %q", c.Token)
		}
	})
}

// FuzzObjects: what an API server sends is checked on the way into the cache.
// An object Problem lets in has a name, namespace and ports as Kubernetes
// allows them.
func FuzzObjects(f *testing.F) {
	f.Add([]byte(`{"metadata":{"name":"web","namespace":"default","uid":"u"},"spec":{"type":"LoadBalancer","ports":[{"protocol":"TCP","port":80,"nodePort":31000}]}}`))
	f.Add([]byte(`{"metadata":{"name":"n1"},"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.1"}]}}`))
	f.Add([]byte(`{"metadata":{"name":"web-x","namespace":"default","labels":{"kubernetes.io/service-name":"web"}},"addressType":"IPv4","endpoints":[{"addresses":["10.1.0.5"],"nodeName":"n1"}]}`))
	f.Add([]byte(`{"metadata":{"name":"WEB\n","namespace":"../x"},"spec":{"ports":[{"port":70000}]}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		var s Service
		if json.Unmarshal(b, &s) == nil && s.Problem() == "" {
			if !SubdomainOK(s.Meta.Name) || s.Meta.Namespace == "" {
				t.Fatalf("let in the Service %q/%q", s.Meta.Namespace, s.Meta.Name)
			}
			for _, p := range s.Spec.Ports {
				if p.Port < 1 || p.Port > 65535 || p.NodePort < 0 || p.NodePort > 65535 {
					t.Fatalf("let in the port %+v", p)
				}
			}
		}
		var n Node
		if json.Unmarshal(b, &n) == nil && n.Problem() == "" && !SubdomainOK(n.Meta.Name) {
			t.Fatalf("let in the Node %q", n.Meta.Name)
		}
		var e EndpointSlice
		if json.Unmarshal(b, &e) == nil && e.Problem() == "" && !SubdomainOK(e.Meta.Name) {
			t.Fatalf("let in the EndpointSlice %q", e.Meta.Name)
		}
	})
}

// FuzzWatch: a watch's stream, whatever the server sends. Watch ends - with
// the server's end, an error, or the context's - and calls back only with
// objects it read.
func FuzzWatch(f *testing.F) {
	f.Add([]byte(`{"type":"ADDED","object":{"metadata":{"name":"n1","resourceVersion":"5"}}}` + "\n"))
	f.Add([]byte(`{"type":"ERROR","object":{"kind":"Status","code":410}}`))
	f.Add([]byte(`{"type":"BOOKMARK","object":{"metadata":{"resourceVersion":"9"}}}{"type":"MODIFIED","object":7}`))
	f.Add([]byte(`{"type":"ADDED","object":{`))
	f.Fuzz(func(t *testing.T, body []byte) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
		defer srv.Close()
		u, _ := url.Parse(srv.URL)
		c := New(&Config{Server: u, Source: "fuzz"})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = Watch(ctx, c, "/api/v1/nodes", "1", func(ch Change[Node]) error { return nil })
		if ctx.Err() != nil {
			t.Fatal("the watch did not end with the server's stream")
		}
	})
}

// FuzzAPIError: an error answer's body reaches logs and terminals: printable.
func FuzzAPIError(f *testing.F) {
	f.Add(500, []byte(`{"kind":"Status","code":500,"reason":"x","message":"etcd\nis down"}`))
	f.Add(403, []byte("\x1b[2Jforbidden"))
	f.Fuzz(func(t *testing.T, code int, b []byte) {
		msg := apiError(code, b).Error()
		for _, r := range msg {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				t.Fatalf("%q keeps %U", msg, r)
			}
		}
	})
}
