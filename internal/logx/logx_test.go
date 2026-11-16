package logx

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

type hidden struct{}

func (hidden) Redacted() string { return "[redacted]" }
func (hidden) String() string   { return "the-real-value" }

func TestLineFormat(t *testing.T) {
	at := time.Date(2026, 11, 9, 10, 2, 11, 123e6, time.FixedZone("CST", -6*3600))
	got := Line(at, "director", OK, "cluster created", "cluster", "prod", "id", "7f3c91a2")
	want := "2026-11-09T16:02:11.123Z [director] OK cluster created cluster=prod id=7f3c91a2"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestFieldsQuoteAndRedact(t *testing.T) {
	got := Fields("reason", "no network yet", "empty", "", "err", errors.New(`bad "x"`), "secret", hidden{}, "took", 1500*time.Millisecond, "raw", []byte{1, 2, 3}, "dangling")
	want := `reason="no network yet" empty="" err="bad \"x\"" secret=[redacted] took=1.5s raw=(3 bytes) dangling=!MISSING`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestLoggerWritesWholeLinesForEveryModule(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, "thelb")
	l.Now = func() time.Time { return time.Unix(0, 0) }
	d := l.Module("director")
	d.OK("ready")
	d.Warn("not checked", "vip", "10.0.0.10")
	l.Module("socket").Error("refused", "uid", 1000)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	want := []string{
		"1970-01-01T00:00:00.000Z [director] OK ready",
		"1970-01-01T00:00:00.000Z [director] WARN not checked vip=10.0.0.10",
		"1970-01-01T00:00:00.000Z [socket] ERROR refused uid=1000",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", buf.String(), strings.Join(want, "\n"))
	}
}

// TestDebugLinesOnlyWhenOn: DEBUG lines are written only while debugging is on,
// for every logger made from the one it was turned on for.
func TestDebugLinesOnlyWhenOn(t *testing.T) {
	var b strings.Builder
	l := New(&b, "thelb")
	m := l.Module("director")
	m.Debug("connection forwarded", "client", "198.18.0.100:51618")
	if b.Len() != 0 || m.Debugging() {
		t.Fatalf("a DEBUG line with debugging off: %q", b.String())
	}
	l.SetDebug(true)
	m.Debug("connection forwarded", "client", "198.18.0.100:51618")
	if !strings.Contains(b.String(), "[director] DEBUG connection forwarded client=198.18.0.100:51618") {
		t.Errorf("with debugging on: %q", b.String())
	}
}

// TestAValueCannotForgeALine: a value from another node or the API - a newline,
// a carriage return, a terminal's escape - is quoted with its characters
// escaped: one event, one line, nothing for the terminal to run.
func TestAValueCannotForgeALine(t *testing.T) {
	line := Line(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), "thelb", Warn, "join refused\nforged", "node", "web\r2026-11-16T00:00:00.000Z [thelb] OK all fine", "mac", "\x1b[2J", "name", "x\u202ey")
	if strings.ContainsAny(line, "\n\r\x1b\u202e") {
		t.Fatalf("the line holds a control character: %q", line)
	}
	for _, want := range []string{`node="web\r2026`, `mac="\x1b[2J"`, `name="x\u202ey"`, "join refused?forged"} {
		if !strings.Contains(line, want) {
			t.Errorf("%q lacks %q", line, want)
		}
	}
}
