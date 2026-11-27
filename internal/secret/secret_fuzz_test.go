package secret

import (
	"strings"
	"testing"
)

// FuzzParse: a secret file's text. Whatever Parse takes is exactly one theLB
// secret, and writes itself back as the same text.
func FuzzParse(f *testing.F) {
	s, err := Generate()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(s.Encode())
	f.Add(s.Encode() + "\n\n")
	f.Add("thelb1:")
	f.Add("thelb1:" + strings.Repeat("A", 43) + "=")
	f.Add("THELB1:" + strings.TrimPrefix(s.Encode(), "thelb1:"))
	f.Fuzz(func(t *testing.T, text string) {
		got, err := Parse(text)
		if err != nil {
			return
		}
		again, err := Parse(got.Encode())
		if err != nil || again.Encode() != got.Encode() || !strings.HasPrefix(got.Encode(), "thelb1:") {
			t.Fatalf("%q read as %q, which does not read back", text, got.Encode())
		}
	})
}
