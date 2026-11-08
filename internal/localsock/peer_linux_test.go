//go:build linux

package localsock

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// The kernel's own answer: the caller's effective user ID.
func TestPeerUIDIsTheCallersEffectiveUser(t *testing.T) {
	dir := runDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "p.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := net.Dial("unix", filepath.Join(dir, "p.sock"))
		if err == nil {
			defer func() { _ = c.Close() }()
			_, _ = c.Read(make([]byte, 1))
		}
	}()
	c, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	uid, err := peerUID(c)
	if err != nil {
		t.Fatal(err)
	}
	if uid != os.Geteuid() {
		t.Errorf("peer uid %d, want %d", uid, os.Geteuid())
	}
}
