// Package safe is how theLB keeps what it is given from doing more than be
// read: text fit to show on a terminal or in a log line, words fit for a shell
// script, and the checks every command-line argument passes before a command
// reads it.
//
// The rule it serves: whatever comes from outside - a flag, a file, another
// node, the Kubernetes API - is checked where it comes in, and quoted or
// cleaned where it goes out. Checked first; this package is the second line.
package safe

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxArg is the longest command-line argument theLB takes: longer than any
// path, address or pattern it needs.
const MaxArg = 4096

// Arg is why a command-line argument is refused, or nil: one that is not
// UTF-8, holds a control character - a newline, a terminal's escape - or a
// character that reorders text, or is longer than MaxArg. No flag of theLB's
// takes one, and each could reach a file theLB prints, a log line or a
// terminal.
func Arg(a string) error {
	switch {
	case len(a) > MaxArg:
		return fmt.Errorf("is longer than %d bytes", MaxArg)
	case !utf8.ValidString(a):
		return fmt.Errorf("is not UTF-8")
	}
	for _, r := range a {
		if unsafeRune(r) {
			return fmt.Errorf("holds the control character %U", r)
		}
	}
	return nil
}

// unsafeRune is a character that changes what a terminal shows or how a
// line ends: control characters (C0, DEL, C1), the line and paragraph
// separators, and the characters that reorder text (Trojan Source).
func unsafeRune(r rune) bool {
	switch {
	case unicode.IsControl(r):
		return true
	case r == ' ' || r == ' ':
		return true
	case r >= '‪' && r <= '‮', r >= '⁦' && r <= '⁩', r == '‎', r == '‏', r == '؜':
		return true
	case r == utf8.RuneError:
		return true
	}
	return false
}

// Text is s fit to show on a terminal or in a line of a log or a table: each
// unsafe character - see Arg - and each byte that is not UTF-8 a "?".
func Text(s string) string {
	clean := true
	for _, r := range s {
		if unsafeRune(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unsafeRune(r) {
			r = '?'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Short is Text, cut at n characters, with "..." where it was cut.
func Short(s string, n int) string {
	s = Text(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "..."
}

// Shell is s as one word of a POSIX shell script: as it is when it holds only
// characters no shell reads specially, otherwise in single quotes - where a
// shell reads nothing specially - each single quote in it ending the quotes,
// written escaped, and opening them again.
func Shell(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !shellPlain(r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellPlain(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("@%+=:,./_-", r)
}

// IfName is whether s can name a Linux network interface, as theLB takes one:
// 1 to 15 bytes of letters, digits, dots, hyphens and underscores, and not
// "." or "..". The kernel takes more; names like these are what systems use.
func IfName(s string) bool {
	if s == "" || len(s) > 15 || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}
