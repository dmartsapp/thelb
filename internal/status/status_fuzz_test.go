package status

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode"
)

// FuzzText: a status holds what nodes, backends and an API server named
// things. Rendered as tables, it holds no character but newlines that moves
// the cursor, ends a line or reorders text - whatever the names hold.
func FuzzText(f *testing.F) {
	f.Add(`{"cluster":{"name":"prod","leader":"lb-1"},"nodes":[{"node":"lb-1","role":"director","address":"10.0.0.11"}]}`, "")
	f.Add(`{"nodes":[{"node":"x\u001b[2J","hostname":"a\nb"}],"pending":[{"pending":"\u202eevil"}],"services":[{"service":"default/web","vip":"10.0.0.50"}]}`, "all")
	f.Add(`{"offers":[{"name":"\r\n","check":"http:/\u0007"}],"backends":[{"service":"s\u2028"}]}`, "backends")
	f.Fuzz(func(t *testing.T, doc, view string) {
		var s Status
		if json.Unmarshal([]byte(doc), &s) != nil {
			return
		}
		text, err := s.Text(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC), strings.Fields(view)...)
		if err != nil {
			return
		}
		for _, r := range text {
			if r != '\n' && (unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || (r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069')) {
				t.Fatalf("the tables keep %U:\n%q", r, text)
			}
		}
	})
}
