package cli

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dmartsapp/thelb/internal/kube"
)

// fakeCluster is an API server with the objects generate k8s reads.
type fakeCluster struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	objs     map[string]any // by path
	denied   map[string]bool
	requests int
	caPEM    []byte
	// gitVersion is what the API server calls its version, if not v1.36.4.
	gitVersion string
}

func newFakeCluster(t *testing.T) *fakeCluster {
	f := &fakeCluster{t: t, objs: map[string]any{}, denied: map[string]bool{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	f.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	f.objs["/api/v1/namespaces/thelb/secrets/thelb-director-token"] = map[string]any{"data": map[string]string{
		"token": base64.StdEncoding.EncodeToString([]byte("director-token")), "ca.crt": base64.StdEncoding.EncodeToString(f.caPEM),
	}}
	f.objs["/api/v1/namespaces/default"] = map[string]any{}
	return f
}

func (f *fakeCluster) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	auth := r.Header.Get("Authorization")
	if auth != "Bearer admin-token" && auth != "Bearer director-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/version":
		git := "v1.36.4"
		if f.gitVersion != "" {
			git = f.gitVersion
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"major": "1", "minor": "36", "gitVersion": git})
		return
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/selfsubjectaccessreviews"):
		if auth != "Bearer director-token" {
			f.t.Errorf("the token check asked as %q", auth)
		}
		var in struct {
			Spec struct {
				A map[string]string `json:"resourceAttributes"`
			} `json:"spec"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		res := in.Spec.A["resource"]
		if in.Spec.A["subresource"] != "" {
			res += "/" + in.Spec.A["subresource"]
		}
		_, _ = fmt.Fprintf(w, `{"status":{"allowed":%v}}`, !f.denied[in.Spec.A["verb"]+" "+res])
		return
	case r.Method != http.MethodGet:
		f.t.Errorf("generate changed the cluster: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	o, ok := f.objs[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"kind":"Status","code":404,"message":"not found"}`)
		return
	}
	if items, isList := o.([]any); isList {
		o = map[string]any{"metadata": map[string]string{"resourceVersion": "1"}, "items": items}
	}
	_ = json.NewEncoder(w).Encode(o)
}

func (f *fakeCluster) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.requests }

// kubeconfig writes the administrator's kubeconfig - and a context whose user
// signs in with a plugin - and is its path.
func (f *fakeCluster) kubeconfig() string {
	path := filepath.Join(f.t.TempDir(), "config")
	ca := base64.StdEncoding.EncodeToString(f.caPEM)
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: lab
  cluster:
    server: %s
    certificate-authority-data: %s
contexts:
- name: admin@lab
  context:
    cluster: lab
    user: admin
- name: cloud@lab
  context:
    cluster: lab
    user: cloud
current-context: admin@lab
users:
- name: admin
  user:
    token: admin-token
- name: cloud
  user:
    exec:
      command: a-plugin
`, f.srv.URL, ca)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func k8sNodeObj(name string, labels map[string]string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name, "labels": labels}}
}

// asking runs the command line at a terminal, answering answer.
func asking(t *testing.T, answer string, args ...string) (int, string, string) {
	t.Helper()
	savedTerm, savedIn := isTerminal, stdin
	isTerminal = func() bool { return true }
	stdin = strings.NewReader(answer)
	t.Cleanup(func() { isTerminal, stdin = savedTerm, savedIn })
	return as(t, 0, args...)
}

func TestGenerateK8sAsksBeforeReadingTheCluster(t *testing.T) {
	f := newFakeCluster(t)
	cfg := f.kubeconfig()
	savedTerm := isTerminal
	isTerminal = func() bool { return false }
	code, out, errOut := as(t, 0, "generate", "k8s", "kubeconfig", "--from-kubeconfig", cfg)
	isTerminal = savedTerm
	if code != 1 || out != "" || !strings.Contains(errOut, "reading the cluster needs your yes: answer at a terminal, or give --yes") {
		t.Errorf("without a terminal: exit %d, %q, %q", code, out, errOut)
	}
	code, out, errOut = asking(t, "n\n", "generate", "k8s", "kubeconfig", "--from-kubeconfig", cfg)
	if code != 1 || out != "" || !strings.Contains(errOut, "the cluster was not read, and nothing was printed") {
		t.Errorf("answered no: exit %d, %q, %q", code, out, errOut)
	}
	if n := f.count(); n != 0 {
		t.Errorf("%d requests before a yes", n)
	}
	for _, want := range []string{"thelb generate k8s kubeconfig reads the Kubernetes cluster", "  kubeconfig   " + cfg, "  context      admin@lab",
		"  API server   " + f.srv.URL, "  user         admin", "the Secret thelb/thelb-director-token", "Read the cluster? [y/N] "} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the question lacks %q:\n%s", want, errOut)
		}
	}
	code, out, errOut = asking(t, "y\n", "generate", "k8s", "kubeconfig", "--from-kubeconfig", cfg)
	if code != 0 || !strings.Contains(out, `token: "director-token"`) {
		t.Errorf("answered yes: exit %d, %q, %q", code, out, errOut)
	}

	// A user who signs in with a plugin: theLB says to use kubectl's way.
	code, _, errOut = as(t, 0, "generate", "k8s", "kubeconfig", "--from-kubeconfig", cfg, "--from-context", "cloud@lab", "--yes")
	if code != 1 || !strings.Contains(errOut, "signs in with exec, which theLB does not do - leave out --from-kubeconfig") {
		t.Errorf("an exec user: exit %d, %q", code, errOut)
	}
	for _, bad := range [][]string{
		{"generate", "k8s", "node", "cp1", "--from-context", "admin@lab"},
		{"generate", "k8s", "node", "cp1", "--yes"},
		{"generate", "k8s", "node", "cp1", "--from-kubeconfig"},
	} {
		if code, _, _ := as(t, 0, bad...); code != 2 {
			t.Errorf("%v: exit %d, want 2", bad, code)
		}
	}
}

func TestGenerateK8sKubeconfigFromTheCluster(t *testing.T) {
	f := newFakeCluster(t)
	code, out, errOut := as(t, 0, "generate", "k8s", "kubeconfig", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(errOut, "checked: with this kubeconfig, a director reaches the API server and has every permission it needs") {
		t.Errorf("no check said:\n%s", errOut)
	}
	// What it printed is a kubeconfig a director reads.
	path := filepath.Join(t.TempDir(), "thelb.kubeconfig")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := kube.Load(path)
	if err != nil || c.Token != "director-token" || c.Server.String() != f.srv.URL || c.RootCAs == nil {
		t.Errorf("the kubeconfig: %+v %v\n%s", c, err, out)
	}
	if !strings.Contains(out, "# Fitted to the cluster at "+f.srv.URL+", Kubernetes v1.36.4.") {
		t.Errorf("lacks what it was fitted to:\n%s", out)
	}

	f.denied["patch services/status"] = true
	_, _, errOut = as(t, 0, "generate", "k8s", "kubeconfig", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if !strings.Contains(errOut, "warning: the token lacks permissions theLB needs: patch services/status") {
		t.Errorf("a missing permission not said:\n%s", errOut)
	}
	delete(f.objs, "/api/v1/namespaces/thelb/secrets/thelb-director-token")
	code, _, errOut = as(t, 0, "generate", "k8s", "kubeconfig", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if code != 1 || !strings.Contains(errOut, "the cluster has no Secret thelb/thelb-director-token: apply what thelb generate k8s prints first") {
		t.Errorf("no Secret: exit %d, %q", code, errOut)
	}
}

func TestGenerateK8sNodeFromTheCluster(t *testing.T) {
	f := newFakeCluster(t)
	f.objs["/api/v1/nodes/cp1"] = k8sNodeObj("cp1", map[string]string{"node-role.kubernetes.io/control-plane": ""})
	f.objs["/api/v1/nodes/worker1"] = k8sNodeObj("worker1", nil)
	f.objs["/api/v1/nodes"] = []any{k8sNodeObj("cp1", nil), k8sNodeObj("worker1", nil)}
	f.objs["/apis/cilium.io/v2/ciliumnodes/cp1"] = map[string]any{}
	f.objs["/api/v1/services"] = []any{map[string]any{
		"metadata": map[string]any{"name": "web", "namespace": "default"},
		"spec":     map[string]any{"type": "LoadBalancer", "loadBalancerClass": "dmarts.app/thelb"},
	}}
	sh := func(script string) {
		t.Helper()
		c := exec.Command("sh", "-n")
		c.Stdin = strings.NewReader(script)
		if b, err := c.CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v: %s", err, b)
		}
	}

	// cp1: a control-plane node with Cilium, in a cluster without kube-proxy.
	code, out, errOut := as(t, 0, "generate", "k8s", "node", "cp1", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	sh(out)
	for _, want := range []string{"# cp1 is a control-plane node; Cilium runs on it.", "#    cp1 is a control-plane node: left as it is.",
		"#    kube-proxy does not run in this cluster: nothing to do.", `kubectl label node "$node" dmarts.app/thelb-director=true --overwrite`, "annotate service"} {
		if !strings.Contains(out, want) {
			t.Errorf("cp1's script lacks %q:\n%s", want, out)
		}
	}
	for _, not := range []string{`kubectl taint node "$node"`, "patch daemonset", "if kubectl"} {
		if strings.Contains(out, not) {
			t.Errorf("cp1's script has %q:\n%s", not, out)
		}
	}
	if !strings.Contains(errOut, "warning: the Service default/web lacks the annotation service.cilium.io/node-selector") {
		t.Errorf("the Service without the annotation not said:\n%s", errOut)
	}

	// worker1: a worker, with kube-proxy, and no Cilium.
	f.objs["/apis/apps/v1/namespaces/kube-system/daemonsets/kube-proxy"] = map[string]any{}
	code, out, errOut = as(t, 0, "generate", "k8s", "node", "worker1", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	sh(out)
	for _, want := range []string{"# worker1 is a worker; kube-proxy runs in the cluster.", "\nkubectl taint node \"$node\"", "\nkubectl -n kube-system patch daemonset kube-proxy",
		"#    Cilium does not run on worker1: no Service to touch.", "once rebooted"} {
		if !strings.Contains(out, want) {
			t.Errorf("worker1's script lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "annotate service") || strings.Contains(out, "if kubectl") {
		t.Errorf("worker1's script touches Services, or tests what it knows:\n%s", out)
	}

	code, _, errOut = as(t, 0, "generate", "k8s", "node", "nope", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if code != 1 || !strings.Contains(errOut, "the cluster has no node nope - its nodes: cp1, worker1") {
		t.Errorf("a node it lacks: exit %d, %q", code, errOut)
	}
}

func TestGenerateK8sDirectorFromTheCluster(t *testing.T) {
	f := newFakeCluster(t)
	f.objs["/api/v1/nodes"] = []any{k8sNodeObj("cp1", map[string]string{"dmarts.app/thelb-director": "true"}), k8sNodeObj("worker1", nil)}
	args := []string{"generate", "k8s", "director", "--vip", "10.0.0.10", "--port", "7443", "--secret-file", "/etc/thelb/director.secret",
		"--backend-secret-file", "/etc/thelb/backend.secret", "--kubeconfig", "in-cluster", "--metrics", "127.0.0.1:9180", "--from-kubeconfig", f.kubeconfig(), "--yes"}
	code, out, errOut := as(t, 0, args...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(out, "KUBERNETES_SERVICE_HOST") || !strings.Contains(errOut, "note: the DaemonSet runs a director on cp1") {
		t.Errorf("without kube-proxy:\n%s\n%s", out, errOut)
	}
	if strings.Contains(out, "--from-kubeconfig") || strings.Contains(out, "--yes") {
		t.Errorf("generate's own flags went into the director's:\n%s", out)
	}
	f.objs["/apis/apps/v1/namespaces/kube-system/daemonsets/kube-proxy"] = map[string]any{}
	_, out, _ = as(t, 0, args...)
	host, port := strings.TrimPrefix(f.srv.URL, "https://"), ""
	host, port, _ = strings.Cut(host, ":")
	want := fmt.Sprintf("          env:\n            # kube-proxy runs in this cluster, and not on a director's node: a pod\n            # there reaches no Service - the API server's own included - so the\n            # director asks the API server at the address your kubeconfig names.\n            - name: KUBERNETES_SERVICE_HOST\n              value: %q\n            - name: KUBERNETES_SERVICE_PORT\n              value: %q\n          securityContext:", host, port)
	if !strings.Contains(out, want) {
		t.Errorf("with kube-proxy, lacks the API server's address:\n%s", out)
	}
}

func TestGenerateK8sServiceFromTheCluster(t *testing.T) {
	f := newFakeCluster(t)
	f.objs["/apis/apps/v1/namespaces/default/deployments/web"] = map[string]any{"spec": map[string]any{
		"selector": map[string]any{"matchLabels": map[string]string{"tier": "front", "app": "web"}},
		"template": map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"ports": []any{map[string]any{"containerPort": 8080}, map[string]any{"containerPort": 53, "protocol": "UDP"}}},
		}}},
	}}
	f.objs["/api/v1/namespaces/default/pods"] = []any{map[string]any{}}
	code, out, errOut := as(t, 0, "generate", "k8s", "service", "web", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"# The pods it selects, and their ports, are those of the Deployment default/web.", "  selector:\n    app: \"web\"\n    tier: \"front\"\n",
		"    - name: tcp-8080\n      protocol: TCP\n      port: 8080\n      targetPort: 8080\n", "    - name: udp-53\n      protocol: UDP\n      port: 53\n      targetPort: 53\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "from the Deployment default/web: the selector app=web, tier=front; the ports tcp/8080, udp/53") {
		t.Errorf("not said where it took them from:\n%s", errOut)
	}

	// The flags win over the Deployment.
	_, out, _ = as(t, 0, "generate", "k8s", "service", "web", "--port", "80:8080", "--selector", "app=web", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if !strings.Contains(out, "  selector:\n    app: \"web\"\n  ports:") || !strings.Contains(out, "      port: 80\n      targetPort: 8080\n") {
		t.Errorf("the flags lost:\n%s", out)
	}

	// No pods; a Service there of another class; a namespace it lacks.
	delete(f.objs, "/api/v1/namespaces/default/pods")
	f.objs["/api/v1/namespaces/default/services/web"] = map[string]any{"metadata": map[string]any{"name": "web", "namespace": "default"},
		"spec": map[string]any{"type": "LoadBalancer", "loadBalancerClass": "io.cilium/l2"}}
	code, _, errOut = as(t, 0, "generate", "k8s", "service", "web", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if code != 1 || !strings.Contains(errOut, "warning: no pod in default has the labels app=web, tier=front") ||
		!strings.Contains(errOut, "of type LoadBalancer and class io.cilium/l2 - a class Kubernetes does not let change: delete it first") {
		t.Errorf("exit %d:\n%s", code, errOut)
	}
	code, _, errOut = as(t, 0, "generate", "k8s", "service", "web", "--namespace", "shop", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if code != 1 || !strings.Contains(errOut, "the cluster has no namespace shop") {
		t.Errorf("a namespace it lacks: exit %d, %q", code, errOut)
	}
}

// What a broken or hostile API server says never adds a line to what generate
// prints: a script people run, YAML they apply.
func TestGenerateK8sTakesNothingUncheckedFromTheCluster(t *testing.T) {
	f := newFakeCluster(t)
	f.gitVersion = "v1.36.4\nrm -rf /"
	f.objs["/api/v1/nodes/cp1"] = k8sNodeObj("cp1", map[string]string{"node-role.kubernetes.io/control-plane": ""})
	code, out, errOut := as(t, 0, "generate", "k8s", "node", "cp1", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if strings.Contains(out, "\nrm -rf") || !strings.Contains(out, "Kubernetes of a version it names oddly:") {
		t.Errorf("the version's newline reached the script:\n%s", out)
	}

	f.gitVersion = ""
	f.objs["/api/v1/namespaces/thelb/secrets/thelb-director-token"] = map[string]any{"data": map[string]string{
		"token": base64.StdEncoding.EncodeToString([]byte("abc\ncurrent-context: evil")), "ca.crt": base64.StdEncoding.EncodeToString(f.caPEM),
	}}
	code, out, errOut = as(t, 0, "generate", "k8s", "kubeconfig", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if code != 1 || out != "" || !strings.Contains(errOut, "holds something that is not a token") {
		t.Errorf("a token with a newline: exit %d, %q, %q", code, out, errOut)
	}
	// A ca.crt whose base64 holds line breaks - which Go's decoder skips - is
	// written again in one line.
	ca := base64.StdEncoding.EncodeToString(f.caPEM)
	secret := func(ca string) {
		f.objs["/api/v1/namespaces/thelb/secrets/thelb-director-token"] = map[string]any{"data": map[string]string{
			"token": base64.StdEncoding.EncodeToString([]byte("director-token")), "ca.crt": ca,
		}}
	}
	secret(ca[:20] + "\nusers\n" + ca[20:])
	code, out, errOut = as(t, 0, "generate", "k8s", "kubeconfig", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if code != 1 || out != "" || !strings.Contains(errOut, "has no ca.crt it can read") {
		t.Errorf("a CA with more than base64: exit %d, %q, %q", code, out, errOut)
	}
	secret(ca[:20] + "\n" + ca[20:])
	code, out, errOut = as(t, 0, "generate", "k8s", "kubeconfig", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if code != 0 || !strings.Contains(out, "certificate-authority-data: "+ca+"\n") {
		t.Errorf("the CA's line break reached the kubeconfig: exit %d, %s\n%s", code, errOut, out)
	}

	f.objs["/apis/apps/v1/namespaces/default/deployments/web"] = map[string]any{"spec": map[string]any{
		"selector": map[string]any{"matchLabels": map[string]string{"app: x\n  type": "web"}},
	}}
	code, out, errOut = as(t, 0, "generate", "k8s", "service", "web", "--from-kubeconfig", f.kubeconfig(), "--yes")
	if code != 1 || out != "" || !strings.Contains(errOut, "selects its pods by a label theLB does not write: app: x?  type=web - give --selector") {
		t.Errorf("a label with YAML in it: exit %d, %q, %q", code, out, errOut)
	}
}
