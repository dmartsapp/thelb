package kube

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestYAMLAsKubectlWritesIt(t *testing.T) {
	in := `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: Q0E=
    server: https://10.0.0.21:6443
  name: kubernetes
contexts:
- context:
    cluster: kubernetes
    user: thelb-director   # a comment
    namespace: "thelb"
  name: thelb
current-context: thelb
kind: Config
preferences: {}
users:
- name: thelb-director
  user:
    token: 'abc''def'
list:
  - a
  - b: c
    d: e
  -
    - nested
`
	got, err := decodeYAML([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"apiVersion": "v1",
		"clusters": []any{map[string]any{
			"cluster": map[string]any{"certificate-authority-data": "Q0E=", "server": "https://10.0.0.21:6443"},
			"name":    "kubernetes",
		}},
		"contexts": []any{map[string]any{
			"context": map[string]any{"cluster": "kubernetes", "user": "thelb-director", "namespace": "thelb"},
			"name":    "thelb",
		}},
		"current-context": "thelb",
		"kind":            "Config",
		"preferences":     map[string]any{},
		"users": []any{map[string]any{
			"name": "thelb-director",
			"user": map[string]any{"token": "abc'def"},
		}},
		"list": []any{"a", map[string]any{"b": "c", "d": "e"}, []any{"nested"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestYAMLRefusesWhatItDoesNotRead(t *testing.T) {
	for _, in := range []string{
		"a: |\n  text\n",
		"a: &x b\n",
		"a: b\n  c: d\n",
		"a:\n\tb: c\n",
		"a: \"open\n",
		"just a line\n",
	} {
		if _, err := decodeYAML([]byte(in)); err == nil {
			t.Errorf("%q: read, should be refused", in)
		}
	}
}

func TestYAMLReadsJSON(t *testing.T) {
	got, err := decodeYAML([]byte(` {"current-context": "x", "n": [1]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if m["current-context"] != "x" {
		t.Fatalf("%#v", got)
	}
}

// testCA is a PEM certificate, for kubeconfigs that need one.
func testCA(t *testing.T, srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

func kubeconfig(server string, ca []byte, user string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: %s
    certificate-authority-data: %s
contexts:
- name: x
  context:
    cluster: c
    user: u
current-context: x
users:
- name: u
  user:
%s
`, server, base64.StdEncoding.EncodeToString(ca), user)
}

func TestKubeconfig(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	ca := testCA(t, srv)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tok"), []byte("filetoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseKubeconfig([]byte(kubeconfig(srv.URL, ca, "    token: abc")), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.String() != srv.URL || c.Token != "abc" || c.RootCAs == nil {
		t.Fatalf("%+v", c)
	}
	c, err = parseKubeconfig([]byte(kubeconfig(srv.URL, ca, "    tokenFile: tok")), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.TokenFile != filepath.Join(dir, "tok") {
		t.Fatalf("tokenFile %q", c.TokenFile)
	}
	if tok, _ := New(c).token(); tok != "filetoken" {
		t.Fatalf("token %q", tok)
	}
	// A kubeconfig naming a theLB secret file as its token file would send the
	// secret to whatever server it names: refused.
	if err := os.WriteFile(filepath.Join(dir, "director.secret"), []byte("thelb1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseKubeconfig([]byte(kubeconfig(srv.URL, ca, "    tokenFile: director.secret")), dir, ""); err == nil || !strings.Contains(err.Error(), "theLB secret") {
		t.Errorf("a theLB secret as the token file: %v", err)
	}
	if _, err := parseKubeconfig([]byte(kubeconfig(srv.URL, ca, "    token: \"a\\nb\"")), dir, ""); err == nil {
		t.Error("a token with a newline: taken")
	}
	for user, says := range map[string]string{
		"    exec:\n      command: aws":    "exec",
		"    username: a\n    password: b": "username",
		"    client-key-data: QQ==":        "without",
		"    {}":                           "",
	} {
		_, err := parseKubeconfig([]byte(kubeconfig(srv.URL, ca, user)), dir, "")
		if err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("%q: %v, want an error saying %q", user, err, says)
		}
	}
	insecure := strings.Replace(kubeconfig(srv.URL, ca, "    token: a"), "    server:", "    insecure-skip-tls-verify: true\n    server:", 1)
	if _, err := parseKubeconfig([]byte(insecure), dir, ""); err == nil || !strings.Contains(err.Error(), "insecure") {
		t.Errorf("insecure-skip-tls-verify: %v", err)
	}
	plain := kubeconfig(strings.Replace(srv.URL, "https", "http", 1), ca, "    token: a")
	if _, err := parseKubeconfig([]byte(plain), dir, ""); err == nil {
		t.Error("an http server: accepted")
	}
}

func TestInCluster(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	dir := t.TempDir()
	for name, b := range map[string][]byte{"ca.crt": testCA(t, srv), "token": []byte("pod"), "namespace": []byte("thelb")} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldDir, oldEnv := saDir, getenv
	defer func() { saDir, getenv = oldDir, oldEnv }()
	saDir = dir
	getenv = func(string) string { return "" }
	if _, err := Load(InCluster); err == nil {
		t.Fatal("outside a pod: loaded")
	}
	getenv = func(k string) string {
		return map[string]string{"KUBERNETES_SERVICE_HOST": "10.96.0.1", "KUBERNETES_SERVICE_PORT": "443"}[k]
	}
	c, err := Load(InCluster)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.String() != "https://10.96.0.1:443" || c.Namespace != "thelb" || c.TokenFile != filepath.Join(dir, "token") {
		t.Fatalf("%+v", c)
	}
}

// fakeAPI is an API server for the client's tests.
type fakeAPI struct {
	t       *testing.T
	mu      sync.Mutex
	srv     *httptest.Server
	got     []string // method path?query, of each request
	bodies  map[string]string
	watches chan []string // each watch's events, as lines
}

func newFakeAPI(t *testing.T, h func(f *fakeAPI, w http.ResponseWriter, r *http.Request) bool) (*fakeAPI, *Client) {
	f := &fakeAPI{t: t, bodies: map[string]string{}, watches: make(chan []string, 10)}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		line := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			line += "?" + r.URL.RawQuery
		}
		f.got = append(f.got, line)
		f.bodies[r.Method+" "+r.URL.Path] = r.Header.Get("Content-Type") + " " + string(b)
		f.mu.Unlock()
		if h(f, w, r) {
			return
		}
		if r.URL.Query().Get("watch") == "1" {
			w.Header().Set("Content-Type", "application/json")
			select {
			case evs := <-f.watches:
				for _, e := range evs {
					_, _ = io.WriteString(w, e+"\n")
					w.(http.Flusher).Flush()
				}
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"kind":"Status","code":404,"message":"nothing here"}`)
	}))
	t.Cleanup(f.srv.Close)
	u, _ := url.Parse(f.srv.URL)
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	return f, New(&Config{Server: u, RootCAs: pool, Token: "tok"})
}

func TestClientRequests(t *testing.T) {
	f, c := newFakeAPI(t, func(f *fakeAPI, w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/version":
			_, _ = io.WriteString(w, `{"major":"1","minor":"36+","gitVersion":"v1.36.4"}`)
		case "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
			b := f.bodies["POST "+r.URL.Path]
			_, _ = fmt.Fprintf(w, `{"status":{"allowed":%v}}`, strings.Contains(b, `"verb":"list"`))
		case "/api/v1/namespaces/default/services/web/status", "/apis/events.k8s.io/v1/namespaces/default/events":
			_, _ = io.WriteString(w, `{}`)
		default:
			return false
		}
		return true
	})
	ctx := context.Background()
	minor, git, err := c.Version(ctx)
	if err != nil || minor != 36 || git != "v1.36.4" {
		t.Fatalf("version %d %q %v", minor, git, err)
	}
	if ok, err := c.Allowed(ctx, Access{Resource: "services", Verb: "list"}); err != nil || !ok {
		t.Fatalf("list services: %v %v", ok, err)
	}
	if ok, err := c.Allowed(ctx, Access{Resource: "services", Subresource: "status", Verb: "patch"}); err != nil || ok {
		t.Fatalf("patch services/status: %v %v", ok, err)
	}
	if err := c.SetIngress(ctx, "default", "web", []Ingress{{IP: "10.0.0.50", IPMode: "VIP"}}); err != nil {
		t.Fatal(err)
	}
	body := f.bodies["PATCH /api/v1/namespaces/default/services/web/status"]
	if body != `application/merge-patch+json {"status":{"loadBalancer":{"ingress":[{"ip":"10.0.0.50","ipMode":"VIP"}]}}}` {
		t.Fatalf("patch %s", body)
	}
	if err := c.SetIngress(ctx, "default", "web", nil); err != nil {
		t.Fatal(err)
	}
	if body := f.bodies["PATCH /api/v1/namespaces/default/services/web/status"]; !strings.HasSuffix(body, `{"ingress":[]}}}`) {
		t.Fatalf("clearing: %s", body)
	}
	if err := c.Emit(ctx, Event{Service: Meta{Name: "web", Namespace: "default", UID: "u1"}, Reason: "IPAllocated", Action: "Allocate", Note: "10.0.0.50", Instance: "lb1", Timestamp: time.Unix(0, 0)}); err != nil {
		t.Fatal(err)
	}
	var ev map[string]any
	_ = json.Unmarshal([]byte(strings.SplitN(f.bodies["POST /apis/events.k8s.io/v1/namespaces/default/events"], " ", 2)[1]), &ev)
	if ev["reason"] != "IPAllocated" || ev["type"] != "Normal" || ev["eventTime"] != "1970-01-01T00:00:00.000000Z" {
		t.Fatalf("event %v", ev)
	}
	if _, err := c.Node(ctx, "nope"); !IsNotFound(err) || !strings.Contains(err.Error(), "nothing here") {
		t.Fatalf("a missing node: %v", err)
	}
	c.cfg.Token = "wrong"
	if _, _, err := c.Version(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a wrong token: %v", err)
	}
}

func TestListPagesAndWatch(t *testing.T) {
	f, c := newFakeAPI(t, func(f *fakeAPI, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/nodes" || r.URL.Query().Get("watch") == "1" {
			return false
		}
		if r.URL.Query().Get("continue") == "" {
			_, _ = io.WriteString(w, `{"metadata":{"resourceVersion":"","continue":"p2"},"items":[{"metadata":{"name":"a"}}]}`)
		} else {
			_, _ = io.WriteString(w, `{"metadata":{"resourceVersion":"7"},"items":[{"metadata":{"name":"b"}}]}`)
		}
		return true
	})
	ctx := context.Background()
	nodes, rv, err := List[Node](ctx, c, "/api/v1/nodes")
	if err != nil || rv != "7" || len(nodes) != 2 || nodes[1].Meta.Name != "b" {
		t.Fatalf("%v %q %v", nodes, rv, err)
	}
	f.watches <- []string{
		`{"type":"ADDED","object":{"metadata":{"name":"c","resourceVersion":"8"}}}`,
		`{"type":"BOOKMARK","object":{"metadata":{"resourceVersion":"9"}}}`,
		`{"type":"DELETED","object":{"metadata":{"name":"a","resourceVersion":"10"}}}`,
		`{"type":"ERROR","object":{"kind":"Status","code":410,"reason":"Expired","message":"too old"}}`,
	}
	var seen []string
	rv, err = Watch(ctx, c, "/api/v1/nodes", rv, func(ch Change[Node]) error {
		seen = append(seen, ch.Type+" "+ch.Object.Meta.Name)
		return nil
	})
	if !IsGone(err) || rv != "10" || strings.Join(seen, ",") != "ADDED c,DELETED a" {
		t.Fatalf("%v %q %v", seen, rv, err)
	}
	if want := "GET /api/v1/nodes?allowWatchBookmarks=true&resourceVersion=7&timeoutSeconds=300&watch=1"; f.got[len(f.got)-1] != want {
		t.Fatalf("watch request %q", f.got[len(f.got)-1])
	}
}

func TestCacheListsWatchesAndListsAgain(t *testing.T) {
	oldMin := backoffMin
	backoffMin = 10 * time.Millisecond
	defer func() { backoffMin = oldMin }()
	var lists int
	var mu sync.Mutex
	f, c := newFakeAPI(t, func(f *fakeAPI, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("watch") == "1" {
			return false
		}
		mu.Lock()
		lists++
		n := lists
		mu.Unlock()
		if n == 1 {
			_, _ = io.WriteString(w, `{"metadata":{"resourceVersion":"1"},"items":[{"metadata":{"name":"a"}}]}`)
		} else {
			_, _ = io.WriteString(w, `{"metadata":{"resourceVersion":"5"},"items":[{"metadata":{"name":"z"}}]}`)
		}
		return true
	})
	k := NewCache(c, "/api/v1/nodes", func(n *Node) string { return n.Meta.Key() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := k.Changed()
	go k.Run(ctx)
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("waiting for %s: items %v", what, keys(k.Items()))
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	<-changed
	waitFor("the first list", func() bool { return k.Synced() && k.Get("a") != nil })
	f.watches <- []string{`{"type":"ADDED","object":{"metadata":{"name":"b","resourceVersion":"2"}}}`}
	waitFor("the watched node", func() bool { return k.Get("b") != nil })
	f.watches <- []string{`{"type":"ERROR","object":{"kind":"Status","code":410,"message":"too old"}}`}
	waitFor("the second list", func() bool { return k.Get("z") != nil && k.Get("a") == nil && k.Get("b") == nil })
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The kubeconfig thelb generate k8s kubeconfig prints, as theLB reads it.
func TestTheGeneratedKubeconfig(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	in := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
  - name: cluster
    cluster:
      server: %s
      certificate-authority-data: %s
users:
  - name: thelb-director
    user:
      token: eyJhbGciOi.payload.sig
contexts:
  - name: thelb
    context:
      cluster: cluster
      user: thelb-director
current-context: thelb
`, srv.URL, base64.StdEncoding.EncodeToString(testCA(t, srv)))
	c, err := parseKubeconfig([]byte(in), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "eyJhbGciOi.payload.sig" || c.Server.String() != srv.URL {
		t.Fatalf("%+v", c)
	}
}

func TestLoadContextNamesWhichContext(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	ca := testCA(t, srv)
	two := kubeconfig(srv.URL, ca, "    token: abc") + `- name: other
  user:
    exec:
      command: some-plugin
`
	two = strings.Replace(two, "contexts:\n", "contexts:\n- name: plugged\n  context:\n    cluster: c\n    user: other\n", 1)
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(two), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadContext(path, "")
	if err != nil || c.Context != "x" || c.User != "u" || c.Token != "abc" {
		t.Fatalf("the current context: %+v %v", c, err)
	}
	_, err = LoadContext(path, "plugged")
	var ae *AuthError
	if !errors.As(err, &ae) || ae.User != "other" || ae.Method != "exec" {
		t.Errorf("an exec user: %v", err)
	}
	if _, err := LoadContext(path, "nope"); err == nil || !strings.Contains(err.Error(), `no context named "nope"`) {
		t.Errorf("a context it lacks: %v", err)
	}
}
