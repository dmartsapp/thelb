// Package logx writes theLB's log lines, one per event:
//
//	<time, RFC 3339 in UTC> [<module>] <OK|WARN|ERROR> <what happened> key=value ...
//
// - the grammar of shint's log lines, with a timestamp a machine can sort. Values
// with spaces or quotes are quoted, and a value that knows it must not be logged
// (a secret) is written as [redacted], whatever the caller passes.
package logx

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Level is how a line reads at a glance.
type Level string

// The levels.
const (
	OK    Level = "OK"
	Warn  Level = "WARN"
	Error Level = "ERROR"
)

// Redactor is implemented by values that must never be logged as they are; the
// log writes what Redacted returns instead.
type Redactor interface {
	Redacted() string
}

// Logger writes lines for one module. Loggers made from one another with Module
// share their writer, and never interleave a line.
type Logger struct {
	mu     *sync.Mutex
	w      io.Writer
	module string
	// Now is the clock; tests replace it.
	Now func() time.Time
}

// New returns a logger writing to w, for module.
func New(w io.Writer, module string) *Logger {
	return &Logger{mu: &sync.Mutex{}, w: w, module: module, Now: time.Now}
}

// Module returns a logger for another module, writing to the same place.
func (l *Logger) Module(module string) *Logger {
	c := *l
	c.module = module
	return &c
}

// OK logs an event that went as it should.
func (l *Logger) OK(msg string, kv ...any) { l.log(OK, msg, kv) }

// Warn logs an event that deserves attention, but does not stop anything.
func (l *Logger) Warn(msg string, kv ...any) { l.log(Warn, msg, kv) }

// Error logs a failure.
func (l *Logger) Error(msg string, kv ...any) { l.log(Error, msg, kv) }

func (l *Logger) log(level Level, msg string, kv []any) {
	s := Line(l.Now(), l.module, level, msg, kv...)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.w, s+"\n")
}

// Line formats one log line.
func Line(t time.Time, module string, level Level, msg string, kv ...any) string {
	s := t.UTC().Format("2006-01-02T15:04:05.000Z07:00") + " [" + module + "] " + string(level) + " " + msg
	if f := Fields(kv...); f != "" {
		s += " " + f
	}
	return s
}

// Fields renders key=value pairs. A key without a value is written as key=!MISSING
// rather than dropped, so a mistake shows.
func Fields(kv ...any) string {
	parts := make([]string, 0, (len(kv)+1)/2)
	for i := 0; i < len(kv); i += 2 {
		key := fmt.Sprint(kv[i])
		if i+1 >= len(kv) {
			parts = append(parts, key+"=!MISSING")
			break
		}
		parts = append(parts, key+"="+value(kv[i+1]))
	}
	return strings.Join(parts, " ")
}

func value(v any) string {
	var s string
	switch x := v.(type) {
	case Redactor:
		return x.Redacted()
	case error:
		s = x.Error()
	case time.Duration:
		s = x.String()
	case time.Time:
		s = x.UTC().Format(time.RFC3339)
	case []byte:
		return fmt.Sprintf("(%d bytes)", len(x))
	case string:
		s = x
	default:
		s = fmt.Sprint(x)
	}
	if s == "" || strings.ContainsAny(s, " \t\n\"=") {
		return strconv.Quote(s)
	}
	return s
}
