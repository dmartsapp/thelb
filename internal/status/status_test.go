package status

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 9, 10, 40, 0, 0, time.UTC)

func sample() Status {
	created := time.Date(2026, 11, 9, 10, 2, 11, 0, time.UTC)
	seen := now.Add(-1100 * time.Millisecond)
	svc := "nginx"
	return Status{
		View:    "director",
		Cluster: &Cluster{Name: "prod", ID: "7f3c91a2", Created: created, Leader: "lb-1", LeaderSince: created},
		Nodes: []Node{
			{Node: "lb-1", Role: "leader", Address: "10.0.0.11", State: "up", Version: "0.1.0", NodeFingerprint: "7c41e0a2f39b55d1"},
			{Node: "web-1", Role: "backend", Address: "10.0.0.21", State: "up", Version: "0.1.0", NodeFingerprint: "0be98d17c46a2f83", LastCheckIn: &seen},
		},
		Certificates: []Certificate{{Certificate: "theLB prod CA", Kind: "ca", Fingerprint: strings.Repeat("ab", 32), NotAfter: created.AddDate(10, 0, 0)}},
		Pools:        []Pool{{Pool: "external", Network: "10.10.1.0/24", First: "10.10.1.20", Count: 10, Interface: "eth0", Used: 1}},
		Offers:       []Offer{{Node: "web-1", Address: "10.0.0.21", Protocol: "tcp", Port: 8080, Check: "http /healthz", Service: &svc, SinceSeconds: 7200}},
	}
}

func TestTextShowsEveryTableInOrder(t *testing.T) {
	text, err := sample().Text(now)
	if err != nil {
		t.Fatal(err)
	}
	var heads []string
	for _, block := range strings.Split(text, "\n\n") {
		heads = append(heads, strings.Fields(block)[0])
	}
	want := []string{"cluster", "node", "certificate", "pending", "pool", "node", "vip", "service", "service"}
	if strings.Join(heads, " ") != strings.Join(want, " ") {
		t.Errorf("tables %v, want %v", heads, want)
	}
	for _, s := range []string{
		" prod    | 7f3c91a2 | 2026-11-09 10:02:11 | lb-1   |    - | 2026-11-09 10:02:11",
		"| 1.1 s ago",
		" external | 10.10.1.0/24 | 10.10.1.20 - 10.10.1.29 | eth0      | 1 of 10",
		"| tcp/8080 | http /healthz | nginx   | 2 h",
		" theLB prod CA | ca   | abababababababab |",
		"(0 rows)",
	} {
		if !strings.Contains(text, s) {
			t.Errorf("text lacks %q:\n%s", s, text)
		}
	}
}

func TestTextPicksTables(t *testing.T) {
	text, err := sample().Text(now, "nodes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, " node ") || strings.Contains(text, "cluster") {
		t.Errorf("nodes alone:\n%s", text)
	}
	if _, err := sample().Text(now, "secrets"); err == nil {
		t.Error("an unknown table was shown")
	}
}

func TestJSONHasAListForEveryTable(t *testing.T) {
	s := Status{View: "director"}
	s.Normalize()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{`"nodes":[]`, `"certificates":[]`, `"pending":[]`, `"pools":[]`, `"offers":[]`, `"vips":[]`, `"services":[]`, `"backends":[]`, `"cluster":null`} {
		if !strings.Contains(string(b), name) {
			t.Errorf("JSON lacks %s: %s", name, b)
		}
	}
}

func TestByteCounts(t *testing.T) {
	for n, want := range map[uint64]string{0: "0 B", 999: "999 B", 1000: "1.0 kB", 1181116006: "1.2 GB"} {
		if got := bytesText(n); got != want {
			t.Errorf("bytesText(%d) = %q, want %q", n, got, want)
		}
	}
}
