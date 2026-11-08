// Package secret holds the cluster's secrets: one for directors, one for
// backends. A secret is 256 random bits, written as one line,
//
//	thelb1:<the 32 bytes, base64url without padding>
//
// read only from a file - never from the command line, where other users could
// see it in the process list - and printed only where a command exists to print
// one. Everywhere else - fmt, JSON, the log - a Secret shows as [redacted].
package secret

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Size is a secret's length in bytes: 256 bits.
const Size = 32

const prefix = "thelb1:"

// maxFile bounds what Load reads: a secret file is one short line.
const maxFile = 4096

// Secret is one cluster secret. Its zero value is no secret.
type Secret struct {
	b   [Size]byte
	set bool
}

// Generate returns a new random secret.
func Generate() (Secret, error) {
	var s Secret
	if _, err := rand.Read(s.b[:]); err != nil {
		return Secret{}, fmt.Errorf("generating a secret: %w", err)
	}
	s.set = true
	return s, nil
}

// Parse reads a secret from its text form. It refuses anything but a theLB
// secret of exactly 256 bits.
func Parse(text string) (Secret, error) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, prefix) {
		return Secret{}, fmt.Errorf("not a theLB secret: it should start with %q", prefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(t, prefix))
	if err != nil {
		return Secret{}, errors.New("not a theLB secret: its value is not base64url")
	}
	if len(raw) != Size {
		return Secret{}, fmt.Errorf("not a theLB secret: %d bits, and a secret is %d", len(raw)*8, Size*8)
	}
	var s Secret
	copy(s.b[:], raw)
	s.set = true
	return s, nil
}

// IsZero reports whether s holds no secret.
func (s Secret) IsZero() bool { return !s.set }

// Encode returns the secret's text form - the one way to see it.
func (s Secret) Encode() string { return prefix + base64.RawURLEncoding.EncodeToString(s.b[:]) }

// Bytes returns a copy of the secret's bytes, for keying an HMAC.
func (s Secret) Bytes() []byte {
	b := make([]byte, Size)
	copy(b, s.b[:])
	return b
}

// Equal compares two secrets in constant time.
func (s Secret) Equal(o Secret) bool {
	return s.set && o.set && subtle.ConstantTimeCompare(s.b[:], o.b[:]) == 1
}

// String, GoString, Redacted and the marshallers keep a secret out of
// everything that prints a value.
func (Secret) String() string                { return "[redacted]" }
func (Secret) GoString() string              { return "secret.Secret{[redacted]}" }
func (Secret) Redacted() string              { return "[redacted]" }
func (Secret) MarshalJSON() ([]byte, error)  { return []byte(`"[redacted]"`), nil }
func (Secret) MarshalText() ([]byte, error)  { return []byte("[redacted]"), nil }
func (*Secret) UnmarshalJSON(_ []byte) error { return errors.New("a secret is never read from JSON") }
func (*Secret) UnmarshalText(_ []byte) error {
	return errors.New("a secret is never read from text this way")
}

// CheckOwner refuses a secret file that root does not own. It is a variable so
// that tests, which do not run as root, can relax it.
var CheckOwner = func(fi os.FileInfo) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 {
		return fmt.Errorf("owned by uid %d, not by root", st.Uid)
	}
	return nil
}

// Exists reports whether path exists - as anything, a symbolic link included.
func Exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("secret file %s: %w", path, err)
}

// Load reads a secret file. It must be a regular file - not a symbolic link -
// owned by root and readable by no one else, holding one theLB secret.
func Load(path string) (Secret, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Secret{}, fmt.Errorf("secret file %s does not exist", path)
		}
		return Secret{}, fmt.Errorf("secret file %s: %w", path, err)
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return Secret{}, fmt.Errorf("secret file %s is a symbolic link: theLB reads only a regular file", path)
	case !fi.Mode().IsRegular():
		return Secret{}, fmt.Errorf("secret file %s is not a regular file", path)
	case fi.Mode().Perm()&0o077 != 0:
		return Secret{}, fmt.Errorf("secret file %s can be read by group or others (mode %04o): run chmod 600 %s", path, fi.Mode().Perm(), path)
	case fi.Size() > maxFile:
		return Secret{}, fmt.Errorf("secret file %s is too big to hold a secret", path)
	}
	if err := CheckOwner(fi); err != nil {
		return Secret{}, fmt.Errorf("secret file %s is %v", path, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Secret{}, fmt.Errorf("secret file %s: %w", path, err)
	}
	s, err := Parse(string(b))
	if err != nil {
		return Secret{}, fmt.Errorf("secret file %s: %w", path, err)
	}
	return s, nil
}

// CreateExclusive writes s to a new file at path, readable by its owner only.
// It never overwrites: if anything exists at path - a file, a directory, a
// symbolic link - it fails. It does not create directories.
func CreateExclusive(path string, s Secret) error {
	if s.IsZero() {
		return errors.New("refusing to write an empty secret")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("secret file %s already exists: theLB never overwrites one", path)
		}
		return fmt.Errorf("creating secret file %s: %w", path, err)
	}
	_, werr := f.WriteString(s.Encode() + "\n")
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("writing secret file %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secret file %s: %w", path, err)
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir makes a new file's directory entry durable; a failure here is not
// worth failing for, the file itself is.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
