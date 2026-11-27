package safe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzText: whatever the cluster, a node or an API server says, Text makes it
// fit for a terminal and a log line - UTF-8, and no character that moves the
// cursor, ends a line or reorders text.
func FuzzText(f *testing.F) {
	for _, s := range []string{"web-1", "a\nb", "\x1b[2J", "\u202ereversed", "\xff\xfe", "x\u2028y", "\u0085", ""} {
		f.Add(s, 10)
	}
	f.Fuzz(func(t *testing.T, s string, n int) {
		for _, out := range []string{Text(s), Short(s, max(0, n%300))} {
			if !utf8.ValidString(out) {
				t.Fatalf("%q -> %q is not UTF-8", s, out)
			}
			for _, r := range out {
				if unsafeRune(r) {
					t.Fatalf("%q -> %q keeps %U", s, out, r)
				}
			}
		}
		if Arg(s) == nil && Text(s) != s {
			t.Fatalf("an argument Arg takes, %q, is changed by Text", s)
		}
	})
}

// FuzzShell: Shell's word is one word a POSIX shell reads back as s - its
// characters plain, or in single quotes with each quote written outside them.
func FuzzShell(f *testing.F) {
	for _, s := range []string{"web", "", "a b", "it's", "$(id)", "`id`", "a\nb", "''", "-rf", "~root"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		w := Shell(s)
		if got, ok := unshell(w); !ok || got != s {
			t.Fatalf("Shell(%q) = %s, which a shell reads as %q", s, w, got)
		}
	})
}

// unshell reads one word as a POSIX shell does, for the characters Shell
// writes: plain ones, single-quoted runs, and \' outside quotes.
func unshell(w string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(w); {
		switch c := w[i]; {
		case c == '\'':
			end := strings.IndexByte(w[i+1:], '\'')
			if end < 0 {
				return "", false
			}
			b.WriteString(w[i+1 : i+1+end])
			i += end + 2
		case c == '\\' && i+1 < len(w) && w[i+1] == '\'':
			b.WriteByte('\'')
			i += 2
		case shellPlain(rune(c)):
			b.WriteByte(c)
			i++
		default:
			return "", false // a character a shell would read specially
		}
	}
	return b.String(), true
}
