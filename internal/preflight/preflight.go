// Package preflight checks, before theLB changes anything, that it has every
// authority it needs - full authority, or it does not start.
package preflight

import "strings"

// Result is what preflight found: the kernel, and the features this node lacks
// that theLB can do without - each refusing only what needs it.
type Result struct {
	Found string
	Lacks map[string]string // by feature - "sctp", "nftables" - why, and what it refuses
}

// Missing is one thing theLB needs and lacks, and how to give it.
type Missing struct {
	What string
	Fix  string
}

// Error is everything missing.
type Error []Missing

func (e Error) Error() string {
	var b strings.Builder
	b.WriteString("this node lacks what theLB needs, and theLB changed nothing:")
	for _, m := range e {
		b.WriteString("\n  - " + m.What + ": " + m.Fix)
	}
	return b.String()
}
