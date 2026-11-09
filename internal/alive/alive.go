// Package alive tells systemd's watchdog that theLB is alive only while the
// loops that keep it working go round: each loop ticks as it goes, saying when
// it is due again; one that is overdue stops the telling - and systemd, which
// then hears nothing, restarts theLB. A loop that ends as it should says so,
// and is no longer due.
package alive

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/dmartsapp/thelb/internal/logx"
)

// Watch is the loops, and when each is due.
type Watch struct {
	log *logx.Logger
	now func() time.Time

	mu    sync.Mutex
	due   map[string]time.Time
	stuck string // the loop last found overdue, or ""
}

// New is a watch with no loop yet.
func New(log *logx.Logger) *Watch {
	return &Watch{log: log, now: time.Now, due: map[string]time.Time{}}
}

// Tick says the loop went round, and is due again within d.
func (w *Watch) Tick(name string, d time.Duration) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.due[name] = w.now().Add(d)
}

// Done says the loop ended as it should: it is no longer due.
func (w *Watch) Done(name string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.due, name)
}

// Overdue is the loop most overdue, and by how much; ok is false when none is.
func (w *Watch) Overdue() (name string, late time.Duration, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	names := make([]string, 0, len(w.due))
	for n := range w.due {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if l := now.Sub(w.due[n]); l > 0 && l > late {
			name, late, ok = n, l, true
		}
	}
	return name, late, ok
}

// Feed tells systemd "WATCHDOG=1" every interval while no loop is overdue,
// until ctx is done - and says, once, when a loop stops and when they all run
// again. Without systemd's watchdog (interval 0), it only says so, every 5 s.
func (w *Watch) Feed(ctx context.Context, interval time.Duration, notify func(string) error) {
	every := interval
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		w.feedOnce(interval > 0, notify)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Watch) feedOnce(watched bool, notify func(string) error) {
	name, late, overdue := w.Overdue()
	w.mu.Lock()
	was := w.stuck
	if overdue {
		w.stuck = name
	} else {
		w.stuck = ""
	}
	w.mu.Unlock()
	switch {
	case overdue && was != name:
		if watched {
			w.log.Error("a loop has stopped: systemd's watchdog is told no more, and restarts theLB", "loop", name, "late", late.Round(time.Second))
		} else {
			w.log.Error("a loop has stopped", "loop", name, "late", late.Round(time.Second))
		}
	case !overdue && was != "":
		w.log.OK("the loops run again", "loop", was)
	}
	if watched && !overdue {
		_ = notify("WATCHDOG=1")
	}
}
