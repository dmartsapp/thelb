package secret

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmartsapp/thelb/internal/logx"
)

// The tests do not run as root, so files they create are not root's.
func relaxOwner(t *testing.T) {
	t.Helper()
	saved := CheckOwner
	CheckOwner = func(os.FileInfo) error { return nil }
	t.Cleanup(func() { CheckOwner = saved })
}

func TestGenerateEncodeParse(t *testing.T) {
	s, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	text := s.Encode()
	if !strings.HasPrefix(text, "thelb1:") || len(text) != len("thelb1:")+43 {
		t.Fatalf("encoding %q: want thelb1: and 43 base64url characters", text)
	}
	back, err := Parse("  " + text + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(s) {
		t.Error("a parsed secret differs from the one encoded")
	}
	other, _ := Generate()
	if other.Equal(s) {
		t.Error("two generated secrets are equal")
	}
	if (Secret{}).Equal(Secret{}) {
		t.Error("two empty secrets compare equal")
	}
}

func TestParseRefusesWhatIsNotASecret(t *testing.T) {
	for _, text := range []string{
		"",
		"hunter2",
		"thelb2:" + strings.Repeat("A", 43),
		"thelb1:not base64!",
		"thelb1:" + strings.Repeat("A", 42), // 31 bytes and a bit
		"thelb1:AAAA",                       // 24 bits
		"thelb1:" + strings.Repeat("A", 64), // 48 bytes
	} {
		if _, err := Parse(text); err == nil {
			t.Errorf("Parse(%q) accepted it", text)
		}
	}
}

func TestASecretNeverPrints(t *testing.T) {
	s, _ := Generate()
	raw := s.Encode()
	type holder struct {
		Name   string
		Secret Secret
	}
	outputs := []string{
		fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s %q", s, s, s, s, s),
		fmt.Sprintf("%v %+v %#v", holder{"x", s}, holder{"x", s}, holder{"x", s}),
		logx.Fields("secret", s),
	}
	j, err := json.Marshal(holder{"x", s})
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(j))
	for _, out := range outputs {
		if strings.Contains(out, raw) || strings.Contains(out, strings.TrimPrefix(raw, "thelb1:")) {
			t.Errorf("the secret leaked into %q", out)
		}
		if !strings.Contains(out, "redacted") {
			t.Errorf("no [redacted] in %q", out)
		}
	}
	var back holder
	if err := json.Unmarshal(j, &back); err == nil {
		t.Error("a secret was read back from JSON")
	}
}

func TestCreateExclusiveWritesOnceReadableByOwnerOnly(t *testing.T) {
	relaxOwner(t)
	path := filepath.Join(t.TempDir(), "director.secret")
	s, _ := Generate()
	if err := CreateExclusive(path, s); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %04o, want 0600", fi.Mode().Perm())
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(s) {
		t.Error("the file holds another secret")
	}
	before, _ := os.ReadFile(path)
	other, _ := Generate()
	if err := CreateExclusive(path, other); err == nil || !strings.Contains(err.Error(), "never overwrites") {
		t.Errorf("a second create: %v, want a refusal to overwrite", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("the file was changed")
	}
}

func TestCreateExclusiveRefusesASymlinkAndAMissingDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	link := filepath.Join(dir, "director.secret")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	s, _ := Generate()
	if err := CreateExclusive(link, s); err == nil {
		t.Error("created a secret through a symbolic link")
	}
	if _, err := os.Lstat(target); err == nil {
		t.Error("the link's target was created")
	}
	if err := CreateExclusive(filepath.Join(dir, "no-such-dir", "x.secret"), s); err == nil {
		t.Error("created a directory for a secret file")
	}
	if err := CreateExclusive(filepath.Join(dir, "empty.secret"), Secret{}); err == nil {
		t.Error("wrote an empty secret")
	}
}

func TestLoadRefusesUnsafeFiles(t *testing.T) {
	relaxOwner(t)
	dir := t.TempDir()
	s, _ := Generate()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good", s.Encode()+"\n", 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"group-readable": write("group", s.Encode(), 0o640),
		"world-readable": write("world", s.Encode(), 0o604),
		"symbolic link":  link,
		"a directory":    dir,
		"not a secret":   write("garbage", "password123", 0o600),
		"missing":        filepath.Join(dir, "missing"),
		"too big":        write("big", strings.Repeat("x", maxFile+1), 0o600),
	}
	for name, path := range cases {
		if _, err := Load(path); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	if _, err := Load(good); err != nil {
		t.Errorf("a good file: %v", err)
	}
}

func TestLoadRefusesAFileRootDoesNotOwn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: the files are root's")
	}
	path := filepath.Join(t.TempDir(), "s")
	s, _ := Generate()
	if err := os.WriteFile(path, []byte(s.Encode()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "not by root") {
		t.Errorf("Load of a file not owned by root: %v", err)
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	if ok, err := Exists(filepath.Join(dir, "nope")); ok || err != nil {
		t.Errorf("a missing file: %v %v", ok, err)
	}
	dangling := filepath.Join(dir, "dangling")
	_ = os.Symlink(filepath.Join(dir, "gone"), dangling)
	if ok, err := Exists(dangling); !ok || err != nil {
		t.Errorf("a dangling link counts as existing: %v %v", ok, err)
	}
}
