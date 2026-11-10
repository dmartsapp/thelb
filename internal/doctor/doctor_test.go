package doctor

import (
	"strings"
	"testing"
)

// TestRenderSaysWhatToDo: the findings as a table, then what to do about each
// that warns or fails - and not about those that pass.
func TestRenderSaysWhatToDo(t *testing.T) {
	fs := []Finding{
		{Area: "kernel", Check: "module ip_vs", State: OK, Detail: "loaded", Fix: "never shown"},
		{Area: "firewall", Check: "ufw", State: Warn, Detail: "active", Fix: "sudo ufw allow from 10.0.0.0/24 to any port 7443 proto tcp"},
		{Area: "authority", Check: "capabilities", State: Fail, Detail: "CAP_NET_ADMIN missing", Fix: "run theLB as root"},
	}
	out := Render(fs)
	for _, want := range []string{"area", "module ip_vs", "What to do:", "warn - ufw: sudo ufw allow from 10.0.0.0/24 to any port 7443 proto tcp", "fail - capabilities: run theLB as root"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "never shown") {
		t.Errorf("a passing check's fix is shown:\n%s", out)
	}
	if !Failed(fs) || Failed(fs[:2]) {
		t.Error("Failed is not whether a finding fails")
	}
}
