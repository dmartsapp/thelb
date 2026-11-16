package kube

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheAPIsObjectsAreChecked(t *testing.T) {
	good := Service{Meta: Meta{Name: "web", Namespace: "default", UID: "6f1d2c3a-0000-4000-8000-000000000001"},
		Spec: ServiceSpec{ExternalTrafficPolicy: "Local", HealthCheckNodePort: 30268, IPFamilies: []string{"IPv4"}, Ports: []ServicePort{{Name: "http", Protocol: "TCP", Port: 80, NodePort: 31000}}}}
	if p := good.Problem(); p != "" {
		t.Fatalf("a good Service: %s", p)
	}
	for what, edit := range map[string]func(*Service){
		"a name with a newline":   func(s *Service) { s.Meta.Name = "web\nOK" },
		"a name with an escape":   func(s *Service) { s.Meta.Name = "\x1b[2Jweb" },
		"an uppercase namespace":  func(s *Service) { s.Meta.Namespace = "Default" },
		"a uid with a space":      func(s *Service) { s.Meta.UID = "a b" },
		"a port past 65535":       func(s *Service) { s.Spec.Ports[0].Port = 65616 },
		"a negative node port":    func(s *Service) { s.Spec.Ports[0].NodePort = -1 },
		"a health port past":      func(s *Service) { s.Spec.HealthCheckNodePort = 70000 },
		"an unknown protocol":     func(s *Service) { s.Spec.Ports[0].Protocol = "ICMP" },
		"an unknown policy":       func(s *Service) { s.Spec.ExternalTrafficPolicy = "Everywhere" },
		"an unknown family":       func(s *Service) { s.Spec.IPFamilies = []string{"IPv5"} },
		"a port name with a dot":  func(s *Service) { s.Spec.Ports[0].Name = "a.b" },
		"no namespace":            func(s *Service) { s.Meta.Namespace = "" },
		"a name starting a digit": func(s *Service) { s.Meta.Name = "1web" },
	} {
		s := good
		s.Spec.Ports = append([]ServicePort(nil), good.Spec.Ports...)
		edit(&s)
		if s.Problem() == "" {
			t.Errorf("a Service with %s: kept", what)
		}
	}

	n := Node{Meta: Meta{Name: "k8sworker1.lan"}}
	if p := n.Problem(); p != "" {
		t.Errorf("a good Node: %s", p)
	}
	for _, bad := range []string{"", "Worker", "w\n1", "w_1", strings.Repeat("a", 254)} {
		if (&Node{Meta: Meta{Name: bad}}).Problem() == "" {
			t.Errorf("a Node named %q: kept", bad)
		}
	}
	if (&Node{Meta: Meta{Name: "w1", Namespace: "default"}}).Problem() == "" {
		t.Error("a Node with a namespace: kept")
	}

	node := "k8sworker1"
	es := EndpointSlice{Meta: Meta{Name: "web-abc12", Namespace: "default", Labels: map[string]string{"kubernetes.io/service-name": "web"}}, AddressType: "IPv4", Endpoints: []Endpoint{{NodeName: &node}}}
	if p := es.Problem(); p != "" {
		t.Errorf("a good EndpointSlice: %s", p)
	}
	evil := "w1\x1b]0;x\x07"
	bad := es
	bad.Endpoints = []Endpoint{{NodeName: &evil}}
	if bad.Problem() == "" {
		t.Error("an endpoint on a node named with an escape: kept")
	}
	bad = es
	bad.Meta.Labels = map[string]string{"kubernetes.io/service-name": "web\nx"}
	if bad.Problem() == "" {
		t.Error("a slice of a Service named with a newline: kept")
	}
}

// TestTheCacheLeavesOutWhatIsWrong: an object the API should never send is
// not kept - not on a list, not on a watch - and is reported once.
func TestTheCacheLeavesOutWhatIsWrong(t *testing.T) {
	k := NewCache[Service](nil, "/api/v1/services", func(s *Service) string { return s.Meta.Key() })
	good := Service{Meta: Meta{Name: "web", Namespace: "default"}, Spec: ServiceSpec{Ports: []ServicePort{{Port: 80}}}}
	bad := Service{Meta: Meta{Name: "api", Namespace: "default"}, Spec: ServiceSpec{Ports: []ServicePort{{Port: 0}}}}
	k.replace([]Service{good, bad})
	if items := k.Items(); len(items) != 1 || items["default/web"] == nil {
		t.Fatalf("after the list: %v", keys(items))
	}
	if r := k.Refused(); len(r) != 1 || r["default/api"] == "" {
		t.Errorf("refused: %v", r)
	}
	if r := k.Refused(); len(r) != 0 {
		t.Errorf("refused again: %v", r)
	}
	// web changed into something wrong: gone from the cache, not kept as it was.
	worse := good
	worse.Spec.Ports = []ServicePort{{Port: 80, Protocol: "GRE"}}
	k.apply(Change[Service]{Type: "MODIFIED", Object: worse})
	if items := k.Items(); len(items) != 0 {
		t.Errorf("after a wrong change: %v", keys(items))
	}
	if r := k.Refused(); r["default/web"] == "" {
		t.Errorf("refused: %v", r)
	}
}

func TestKubeconfigFilesAndTokensAreChecked(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, make([]byte, maxFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSmall(big); err == nil {
		t.Error("a file over the limit: read")
	}
	if _, err := readSmall(dir); err == nil {
		t.Error("a directory: read")
	}
	if _, err := readSmall("/dev/zero"); err == nil {
		t.Error("/dev/zero: read")
	}
	for _, ok := range []string{"abc", "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln", "abc'def"} {
		if p := TokenProblem(ok); p != "" {
			t.Errorf("TokenProblem(%q) = %s", ok, p)
		}
	}
	for _, bad := range []string{"", "a b", "a\nb", "thelb1:AAAA", strings.Repeat("a", 16<<10+1), "tok\x7f"} {
		if TokenProblem(bad) == "" {
			t.Errorf("TokenProblem(%q) = \"\"", bad)
		}
	}
	deep := "a:\n"
	for i := 1; i <= maxDepth+1; i++ {
		deep += strings.Repeat(" ", i) + "a:\n"
	}
	deep += strings.Repeat(" ", maxDepth+2) + "b: c\n"
	if _, err := decodeYAML([]byte(deep)); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Errorf("YAML nested past the limit: %v", err)
	}
}
