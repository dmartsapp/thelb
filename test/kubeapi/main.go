// Command kubeapi is a stand-in Kubernetes API server for theLB's end-to-end
// test: the little of the API theLB uses, over TLS, from a state file the test
// edits.
//
//	kubeapi --listen 198.18.0.200:6443 --state state.json --out dir
//
// It writes dir/kubeconfig for the directors, keeps the objects of the state
// file - services, nodes, endpointslices - and sends watchers each change to
// it. What theLB writes it keeps too: a Service's status in dir/status.json,
// events in dir/events.log. Every access review is allowed, but those the
// state file's "deny" lists ("patch services/status").
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The lists the state file holds, by their API path.
var paths = map[string]string{
	"services":       "/api/v1/services",
	"nodes":          "/api/v1/nodes",
	"endpointslices": "/apis/discovery.k8s.io/v1/endpointslices",
}

const token = "e2e-token"

type object = map[string]any

type server struct {
	mu       sync.Mutex
	rv       int
	objects  map[string]map[string]object // by path, by namespace/name
	status   map[string]any               // by service key: what theLB wrote
	deny     []string
	watchers map[string][]chan string
	out      string
}

func main() {
	listen := flag.String("listen", "", "address:port")
	state := flag.String("state", "", "the state file")
	out := flag.String("out", "", "where to write the kubeconfig, status and events")
	flag.Parse()
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || *state == "" || *out == "" {
		log.Fatal("usage: kubeapi --listen <address:port> --state <file> --out <dir>")
	}
	cert, caPEM, err := selfSigned(host)
	if err != nil {
		log.Fatal(err)
	}
	s := &server{objects: map[string]map[string]object{}, status: map[string]any{}, watchers: map[string][]chan string{}, out: *out}
	for _, p := range paths {
		s.objects[p] = map[string]object{}
	}
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: %s
    server: https://%s
  name: e2e
contexts:
- context:
    cluster: e2e
    user: thelb-director
  name: e2e
current-context: e2e
users:
- name: thelb-director
  user:
    token: %s
`, base64.StdEncoding.EncodeToString(caPEM), *listen, token)
	if err := os.WriteFile(filepath.Join(*out, "kubeconfig"), []byte(kubeconfig), 0o600); err != nil {
		log.Fatal(err)
	}
	s.load(*state)
	go func() {
		for range time.Tick(200 * time.Millisecond) {
			s.load(*state)
		}
	}()
	srv := &http.Server{Addr: *listen, Handler: s, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("serving on https://%s", *listen)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

func selfSigned(host string) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kubeapi e2e"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true, IPAddresses: []net.IP{net.ParseIP(host)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func key(o object) string {
	m, _ := o["metadata"].(map[string]any)
	ns, _ := m["namespace"].(string)
	name, _ := m["name"].(string)
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

// load reads the state file and sends each change to the watchers.
func (s *server) load(file string) {
	b, err := os.ReadFile(file)
	if err != nil {
		return
	}
	var st map[string]json.RawMessage
	if json.Unmarshal(b, &st) != nil {
		return // being written: next time
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deny = nil
	_ = json.Unmarshal(st["deny"], &s.deny)
	for kind, path := range paths {
		var list []object
		_ = json.Unmarshal(st[kind], &list)
		seen := map[string]bool{}
		for _, o := range list {
			k := key(o)
			seen[k] = true
			if kind == "services" {
				if v, ok := s.status[k]; ok {
					o["status"] = v
				}
			}
			old, had := s.objects[path][k]
			if had && reflect.DeepEqual(strip(old), strip(o)) {
				continue
			}
			s.rv++
			setRV(o, s.rv)
			s.objects[path][k] = o
			s.send(path, map[bool]string{true: "MODIFIED", false: "ADDED"}[had], o)
		}
		for k, o := range s.objects[path] {
			if !seen[k] {
				delete(s.objects[path], k)
				s.rv++
				setRV(o, s.rv)
				s.send(path, "DELETED", o)
				if kind == "services" {
					delete(s.status, k)
				}
			}
		}
	}
}

// strip is an object without its resource version, to compare.
func strip(o object) object {
	b, _ := json.Marshal(o)
	var c object
	_ = json.Unmarshal(b, &c)
	if m, ok := c["metadata"].(map[string]any); ok {
		delete(m, "resourceVersion")
	}
	return c
}

func setRV(o object, rv int) {
	m, _ := o["metadata"].(map[string]any)
	if m == nil {
		m = map[string]any{}
		o["metadata"] = m
	}
	m["resourceVersion"] = strconv.Itoa(rv)
}

func (s *server) send(path, typ string, o object) {
	b, _ := json.Marshal(map[string]any{"type": typ, "object": o})
	for _, ch := range s.watchers[path] {
		select {
		case ch <- string(b):
		default: // a watcher that does not keep up: it lists again
		}
	}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+token {
		http.Error(w, `{"kind":"Status","code":401,"message":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	b, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/version":
		_, _ = io.WriteString(w, `{"major":"1","minor":"36","gitVersion":"v1.36.0-e2e"}`)
	case strings.HasSuffix(r.URL.Path, "/selfsubjectaccessreviews"):
		var in struct {
			Spec struct {
				A map[string]string `json:"resourceAttributes"`
			} `json:"spec"`
		}
		_ = json.Unmarshal(b, &in)
		res := in.Spec.A["resource"]
		if in.Spec.A["subresource"] != "" {
			res += "/" + in.Spec.A["subresource"]
		}
		s.mu.Lock()
		denied := slices.Contains(s.deny, in.Spec.A["verb"]+" "+res)
		s.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"status":{"allowed":%v}}`, !denied)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/status"):
		s.patchStatus(w, r.URL.Path, b)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events"):
		var ev map[string]any
		_ = json.Unmarshal(b, &ev)
		reg, _ := ev["regarding"].(map[string]any)
		line := fmt.Sprintf("%v %v/%v %v %v\n", ev["type"], reg["namespace"], reg["name"], ev["reason"], ev["note"])
		f, err := os.OpenFile(filepath.Join(s.out, "events.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = f.WriteString(line)
			_ = f.Close()
		}
		_, _ = io.WriteString(w, `{}`)
	case r.URL.Query().Get("watch") == "1":
		s.watch(w, r)
	default:
		s.list(w, r.URL.Path)
	}
}

func (s *server) list(w http.ResponseWriter, path string) {
	s.mu.Lock()
	objs, ok := s.objects[path]
	items := make([]object, 0, len(objs))
	for _, o := range objs {
		items = append(items, o)
	}
	rv := s.rv
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"kind":"Status","code":404,"message":"not here"}`)
		return
	}
	b, _ := json.Marshal(map[string]any{"metadata": map[string]string{"resourceVersion": strconv.Itoa(rv)}, "items": items})
	_, _ = w.Write(b)
}

func (s *server) watch(w http.ResponseWriter, r *http.Request) {
	ch := make(chan string, 256)
	s.mu.Lock()
	s.watchers[r.URL.Path] = append(s.watchers[r.URL.Path], ch)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.watchers[r.URL.Path] = slices.DeleteFunc(s.watchers[r.URL.Path], func(c chan string) bool { return c == ch })
		s.mu.Unlock()
	}()
	w.(http.Flusher).Flush()
	timeout := time.After(5 * time.Minute)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-timeout:
			return
		case ev := <-ch:
			_, _ = io.WriteString(w, ev+"\n")
			w.(http.Flusher).Flush()
		}
	}
}

// patchStatus takes theLB's merge patch of a Service's status.
func (s *server) patchStatus(w http.ResponseWriter, path string, b []byte) {
	parts := strings.Split(strings.Trim(path, "/"), "/") // api v1 namespaces NS services NAME status
	if len(parts) != 7 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	k := parts[3] + "/" + parts[5]
	var patch struct {
		Status any `json:"status"`
	}
	if json.Unmarshal(b, &patch) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[paths["services"]][k]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"kind":"Status","code":404,"message":"no such service"}`)
		return
	}
	s.status[k] = patch.Status
	o["status"] = patch.Status
	s.rv++
	setRV(o, s.rv)
	s.send(paths["services"], "MODIFIED", o)
	all, _ := json.MarshalIndent(s.status, "", "  ")
	_ = os.WriteFile(filepath.Join(s.out, "status.json.tmp"), all, 0o644)
	_ = os.Rename(filepath.Join(s.out, "status.json.tmp"), filepath.Join(s.out, "status.json"))
	_, _ = w.Write([]byte(`{}`))
}
