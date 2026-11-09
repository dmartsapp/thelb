package alive

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/logx"
)

func TestTheWatchdogIsFedOnlyWhileTheLoopsRun(t *testing.T) {
	var out bytes.Buffer
	w := New(logx.New(&out, "thelb"))
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }
	var told []string
	notify := func(s string) error { told = append(told, s); return nil }

	// No loop yet: alive.
	w.feedOnce(true, notify)
	w.Tick("data plane", 10*time.Second)
	w.Tick("check-in", 30*time.Second)
	now = now.Add(5 * time.Second)
	w.feedOnce(true, notify)
	if len(told) != 2 {
		t.Fatalf("told %v", told)
	}

	// The data plane stops going round: systemd is told no more.
	now = now.Add(10 * time.Second)
	if name, late, ok := w.Overdue(); !ok || name != "data plane" || late != 5*time.Second {
		t.Errorf("overdue: %q %v %v", name, late, ok)
	}
	w.feedOnce(true, notify)
	w.feedOnce(true, notify)
	if len(told) != 2 {
		t.Errorf("told while a loop is overdue: %v", told)
	}
	if strings.Count(out.String(), "a loop has stopped") != 1 || !strings.Contains(out.String(), `loop="data plane"`) {
		t.Errorf("the log:\n%s", out.String())
	}

	// It ends as it should - leadership lost: no longer due, and told again.
	w.Done("data plane")
	w.feedOnce(true, notify)
	if len(told) != 3 || !strings.Contains(out.String(), "the loops run again") {
		t.Errorf("told %v, log:\n%s", told, out.String())
	}

	// Without systemd's watchdog, nothing is told, but a stop is still said.
	w.Tick("check-in", time.Second)
	now = now.Add(2 * time.Second)
	w.feedOnce(false, notify)
	if len(told) != 3 || strings.Count(out.String(), "a loop has stopped") != 2 {
		t.Errorf("unwatched: told %v, log:\n%s", told, out.String())
	}
}
