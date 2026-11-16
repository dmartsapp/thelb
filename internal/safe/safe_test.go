package safe

import (
	"os/exec"
	"strings"
	"testing"
)

func TestArg(t *testing.T) {
	for _, ok := range []string{"", "--vip", "/etc/thelb/director.secret", "web|api", "名前", "a b"} {
		if err := Arg(ok); err != nil {
			t.Errorf("Arg(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"a\nb", "a\rb", "\x1b[2J", "a\x00b", "\x7f", "\u0085", "a\u2028b", "\u202eevil", "\u2066x", "\xff", strings.Repeat("a", MaxArg+1)} {
		if err := Arg(bad); err == nil {
			t.Errorf("Arg(%q) = nil, want a refusal", bad)
		}
	}
}

func TestText(t *testing.T) {
	for in, want := range map[string]string{
		"web":                   "web",
		"a\nb":                  "a?b",
		"\x1b]0;owned\x07title": "?]0;owned?title",
		"x\u202ey":              "x?y",
		"bad\xffutf8":           "bad?utf8",
		"名前":                    "名前",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Short("abcdef", 3); got != "abc..." {
		t.Errorf("Short = %q", got)
	}
	if got := Short("a\nb", 10); got != "a?b" {
		t.Errorf("Short = %q", got)
	}
}

// TestShell runs each word through a real shell: it must come out as it went
// in, one word, running nothing.
func TestShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, w := range []string{"web", "10.0.0.1/24", "a b", "it's", "$(touch /tmp/x)", "`id`", "a;b", "a\nb", "", "'", "\\", "*", "~root", "-x", "%s"} {
		q := Shell(w)
		out, err := exec.Command(sh, "-c", `printf '%s' `+q).Output()
		if err != nil {
			t.Fatalf("sh -c printf %s: %v", q, err)
		}
		if string(out) != w {
			t.Errorf("Shell(%q) = %s, which a shell reads as %q", w, q, out)
		}
	}
	for _, plain := range []string{"web", "web-1", "10.0.0.1/24", "tcp/80", "a=b,c"} {
		if Shell(plain) != plain {
			t.Errorf("Shell(%q) = %s, want it as it is", plain, Shell(plain))
		}
	}
}

func TestIfName(t *testing.T) {
	for _, ok := range []string{"eth0", "ens3", "br-lan", "wlp2s0", "thelb0", "veth1234567890a", "eth0.100", "vlan_10"} {
		if !IfName(ok) {
			t.Errorf("IfName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "eth0:1", "a/b", "a b", "eth\n0", "veth1234567890ab", "é", "eth0;id", "eth$0"} {
		if IfName(bad) {
			t.Errorf("IfName(%q) = true", bad)
		}
	}
}
