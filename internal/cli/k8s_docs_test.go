package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/dmartsapp/thelb/internal/version"
)

// docsExamples are the files the documentation shows of thelb generate k8s:
// its output, never written by hand. THELB_WRITE_EXAMPLES=1 go test
// ./internal/cli -run TestTheDocsShowWhatGenerateK8sPrints writes them again.
var docsExamples = []struct {
	file string
	args []string
}{
	{"thelb-k8s.yaml", []string{"generate", "k8s"}},
	{"thelb-director.yaml", []string{"generate", "k8s", "director", "--vip", "10.0.0.10", "--port", "7443",
		"--secret-file", "/etc/thelb/director.secret", "--backend-secret-file", "/etc/thelb/backend.secret",
		"--kubeconfig", "in-cluster", "--metrics", "127.0.0.1:9180"}},
	{"thelb-kubeconfig.sh", []string{"generate", "k8s", "kubeconfig"}},
	{"web-service.yaml", []string{"generate", "k8s", "service", "web"}},
}

func TestTheDocsShowWhatGenerateK8sPrints(t *testing.T) {
	changelog, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^## v(\d+\.\d+\.\d+)\b`).FindSubmatch(changelog)
	if m == nil {
		t.Fatal("CHANGELOG.md names no release")
	}
	saved := version.Version
	version.Version = string(m[1]) // the release the documentation describes
	defer func() { version.Version = saved }()
	write := os.Getenv("THELB_WRITE_EXAMPLES") != ""
	for _, ex := range docsExamples {
		code, out, errOut := as(t, 0, ex.args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", ex.args, code, errOut)
		}
		path := filepath.Join("../../docs/examples", ex.file)
		if write {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != out {
			t.Errorf("docs/examples/%s is not what thelb %v prints - write them again: THELB_WRITE_EXAMPLES=1 go test ./internal/cli -run TestTheDocsShowWhatGenerateK8sPrints", ex.file, ex.args)
		}
	}
}

func TestGenerateK8sService(t *testing.T) {
	code, out, errOut := as(t, 0, "generate", "k8s", "service", "api", "--namespace", "shop", "--port", "443/tcp:8443", "--port", "53/udp",
		"--selector", "app.kubernetes.io/name=api", "--pool", "external", "--ip", "10.0.0.55", "--shared-vip", "front", "--policy", "Cluster")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"  name: api\n", "  namespace: shop\n", `    dmarts.app/thelb-pool: "external"`, `    dmarts.app/thelb-ip: "10.0.0.55"`,
		`    dmarts.app/thelb-shared-vip: "front"`, "  externalTrafficPolicy: Cluster\n", `    app.kubernetes.io/name: "api"`,
		"    - name: tcp-443\n      protocol: TCP\n      port: 443\n      targetPort: 8443\n", "    - name: udp-53\n      protocol: UDP\n      port: 53\n      targetPort: 53\n"} {
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(out) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range [][]string{
		{"generate", "k8s", "service"},
		{"generate", "k8s", "service", "Web"},
		{"generate", "k8s", "service", "web", "--port", "80/http"},
		{"generate", "k8s", "service", "web", "--port", "80", "--port", "80/tcp"},
		{"generate", "k8s", "service", "web", "--policy", "local"},
		{"generate", "k8s", "service", "web", "--selector", "nolabel"},
		{"generate", "k8s", "nothing"},
	} {
		if code, _, _ := as(t, 0, bad...); code != 2 {
			t.Errorf("%v: exit %d, want 2", bad, code)
		}
	}
}
