// Package doctor checks a node for theLB - its kernel, parameters, firewall,
// platform, what else runs there, and the theLB running on it - and says what
// to do. It changes nothing.
package doctor

import (
	"fmt"
	"strings"

	"github.com/dmartsapp/thelb/internal/status"
	"github.com/dmartsapp/thelb/internal/table"
)

// States of a finding.
const (
	OK   = "ok"
	Info = "info"
	Warn = "warn"
	Fail = "fail" // theLB cannot run, or cannot do part of its work
)

// Finding is one check, and what to do about it.
type Finding struct {
	Area   string `json:"area"`
	Check  string `json:"check"`
	State  string `json:"state"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Facts is what doctor knows of this node's theLB.
type Facts struct {
	Roles     []string        // the roles to check for: director, backend
	Running   map[string]bool // the roles running here
	Port      int             // the control port, 0 when not known
	Interface string          // the control VIP's interface, "" when not known
	Status    *status.Status  // the running director's, or nil
}

// Failed is whether any finding is a failure.
func Failed(fs []Finding) bool {
	for _, f := range fs {
		if f.State == Fail {
			return true
		}
	}
	return false
}

// Render is the findings as a table, and then what to do about each that
// warns or fails.
func Render(fs []Finding) string {
	rows := make([][]string, 0, len(fs))
	var todo []string
	for _, f := range fs {
		rows = append(rows, []string{f.Area, f.Check, f.State, f.Detail})
		if f.Fix != "" && (f.State == Warn || f.State == Fail) {
			todo = append(todo, fmt.Sprintf("  %s - %s: %s", f.State, f.Check, f.Fix))
		}
	}
	out := table.Render([]string{"area", "check", "state", "detail"}, rows)
	if len(todo) > 0 {
		out += "\nWhat to do:\n" + strings.Join(todo, "\n") + "\n"
	}
	return out
}
