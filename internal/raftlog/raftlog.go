// Package raftlog writes Raft's own warnings and errors as theLB's log lines.
// What Raft says at lower levels is left out: theLB logs the events that matter
// - elections won, leadership lost, members added - itself. A line Raft repeats
// - a peer that cannot be reached, retried with backoff - is written at most
// every 30 seconds, with how often it was said.
package raftlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/hashicorp/go-hclog"
)

// Every is how often a repeated line is written.
const Every = 30 * time.Second

// benign are warnings Raft gives whenever a new member catches up - every
// director's start, since every start is a new member identity.
var benign = map[string]bool{
	"appendEntries rejected, sending older logs": true,
	"failed to get previous log":                 true,
}

// New is Raft's logger.
func New(log *logx.Logger) hclog.Logger {
	return hclog.New(&hclog.LoggerOptions{
		Name:        "raft",
		Level:       hclog.Warn,
		Output:      &writer{log: log, seen: map[string]*seen{}},
		JSONFormat:  true,
		DisableTime: true,
	})
}

type seen struct {
	at    time.Time
	count int
}

type writer struct {
	log  *logx.Logger
	mu   sync.Mutex
	seen map[string]*seen
}

func (w *writer) Write(p []byte) (int, error) {
	for _, line := range bytes.Split(bytes.TrimSpace(p), []byte("\n")) {
		w.line(line)
	}
	return len(p), nil
}

func (w *writer) line(line []byte) {
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		w.log.Warn(strings.TrimSpace(string(line)))
		return
	}
	level, _ := m["@level"].(string)
	msg, _ := m["@message"].(string)
	if benign[msg] || m["error"] == "transport shutdown" {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if !strings.HasPrefix(k, "@") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	kv := make([]any, 0, 2*len(keys)+2)
	peer := ""
	for _, k := range keys {
		v := fmt.Sprint(m[k])
		kv = append(kv, k, v)
		if k == "peer" || k == "server-id" || k == "id" || k == "address" {
			peer += v
		}
	}

	key := level + "\x00" + msg + "\x00" + peer
	w.mu.Lock()
	s := w.seen[key]
	now := time.Now()
	if s != nil && now.Sub(s.at) < Every {
		s.count++
		w.mu.Unlock()
		return
	}
	if s != nil && s.count > 0 {
		kv = append(kv, "repeated", s.count)
	}
	w.seen[key] = &seen{at: now}
	for k, v := range w.seen {
		if now.Sub(v.at) > 2*Every {
			delete(w.seen, k)
		}
	}
	w.mu.Unlock()

	if level == "error" {
		w.log.Error(msg, kv...)
		return
	}
	w.log.Warn(msg, kv...)
}
