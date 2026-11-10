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
		Offers: []Offer{
			{Node: "web-1", Offer: "shop", Address: "10.0.0.21", Protocol: "tcp", Port: 8080, Check: "http /healthz", OwnCheck: "ok", Service: &svc, SinceSeconds: 7200},
			{Node: "web-1", Offer: "metrics", Address: "10.0.0.21", Protocol: "tcp", Port: 9100, Check: "tcp", OwnCheck: "fail", SinceSeconds: 60},
		},
	}
}

func TestAllShowsEveryTableInOrder(t *testing.T) {
	text, err := sample().Text(now, "all")
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
		" web-1 | shop    | 10.0.0.21 | tcp/8080 | http /healthz | ok        | nginx   | 2 h",
		" web-1 | metrics | 10.0.0.21 | tcp/9100 | tcp           | fail      | -       | 1 min",
		" theLB prod CA | ca   | abababababababab |",
		" waiting\n",  // the pending table, empty: its header and separator
		"-\n\n pool ", // then a blank line before the next table
	} {
		if !strings.Contains(text, s) {
			t.Errorf("text lacks %q:\n%s", s, text)
		}
	}
	if strings.Contains(text, "rows)") || strings.Contains(text, "(1 row)") {
		t.Errorf("a table counts its rows:\n%s", text)
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

func TestTheSummaryIsTheDefault(t *testing.T) {
	st := sample()
	st.Pending = []Pending{{Pending: "web-2", Role: "backend"}, {Pending: "lb-4", Role: "director"}}
	st.Nodes = append(st.Nodes, Node{Node: "web-3", Role: "backend", State: "down"})
	text, err := st.Text(now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		" directors |     1 |  1 |    0 |       1",
		" backends  |     2 |  1 |    1 |       1",
		" services  |     0 |  0 |    0 |       -",
		"More: thelb status directors | backends | vips | all",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the summary lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "web-1") {
		t.Errorf("the summary lists nodes:\n%s", text)
	}
}

func TestViewsShowOnePartEach(t *testing.T) {
	st := sample()
	st.Pending = []Pending{{Pending: "web-2", Role: "backend"}, {Pending: "lb-4", Role: "director"}}
	st.Certificates = append(st.Certificates, Certificate{Certificate: "lb-1", Kind: "director"}, Certificate{Certificate: "web-1", Kind: "backend"})
	directors, _ := st.Text(now, "directors")
	backends, _ := st.Text(now, "backends")
	vips, _ := st.Text(now, "vips")
	for _, c := range []struct {
		view, text string
		has, lacks []string
	}{
		{"directors", directors, []string{" lb-1 ", " lb-4 ", "theLB prod CA"}, []string{" web-1 ", " web-2 ", " pool "}},
		{"backends", backends, []string{" web-1 ", " web-2 ", "tcp/8080"}, []string{" lb-1 ", " lb-4 ", "theLB prod CA", " cluster "}},
		{"vips", vips, []string{" external ", " vip "}, []string{" lb-1 ", " web-1 "}},
	} {
		for _, h := range c.has {
			if !strings.Contains(c.text, h) {
				t.Errorf("%s lacks %q:\n%s", c.view, h, c.text)
			}
		}
		for _, l := range c.lacks {
			if strings.Contains(c.text, l) {
				t.Errorf("%s has %q:\n%s", c.view, l, c.text)
			}
		}
	}
	j, err := st.Pick("backends")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(j)
	if !strings.Contains(string(b), `"offers":[`) || strings.Contains(string(b), `"lb-1"`) || strings.Contains(string(b), `"pools"`) {
		t.Errorf("backends as JSON: %s", b)
	}
}

func TestABackendShowsOnlyItself(t *testing.T) {
	st := Status{View: "backend", Mine: &Mine{Role: "backend", Cluster: "prod", State: "a member of the cluster", NodeFingerprint: "0be98d17c46a2f83",
		Offers: []MineOffer{{Offer: "shop", Port: "tcp/8080", Check: "http /healthz", Own: "ok", Offered: "yes", Service: "nginx"}}}}
	text, err := st.Text(now, "all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, " backend | prod    | a member of the cluster | shop  | tcp/8080 | http /healthz | ok        | yes     | nginx   | 0be98d17c46a2f83") {
		t.Errorf("a backend's status:\n%s", text)
	}
}
