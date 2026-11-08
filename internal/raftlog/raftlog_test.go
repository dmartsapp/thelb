package raftlog

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/dmartsapp/thelb/internal/logx"
)

func TestRaftsLinesBecomeTheLBs(t *testing.T) {
	var b bytes.Buffer
	l := New(logx.New(&b, "raft"))
	l.Info("entering follower state") // left out
	l.Warn("heartbeat timeout reached, starting election", "last-leader-id", "lb-1-aa")
	for range 5 {
		l.Error("failed to contact", "server-id", "lb-2-bb", "error", errors.New("dial tcp: connection refused"))
	}
	l.Error("failed to contact", "server-id", "lb-3-cc", "error", "timeout")
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines:\n%s", len(lines), b.String())
	}
	if !strings.Contains(lines[0], "[raft] WARN heartbeat timeout reached, starting election last-leader-id=lb-1-aa") {
		t.Errorf("line 1: %s", lines[0])
	}
	if !strings.Contains(lines[1], `[raft] ERROR failed to contact error="dial tcp: connection refused" server-id=lb-2-bb`) {
		t.Errorf("line 2: %s", lines[1])
	}
	if !strings.Contains(lines[2], "server-id=lb-3-cc") {
		t.Errorf("line 3, another peer, is not suppressed: %s", lines[2])
	}
}
