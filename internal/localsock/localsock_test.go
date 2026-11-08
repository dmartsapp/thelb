package localsock

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmartsapp/thelb/internal/logx"
	"github.com/dmartsapp/thelb/internal/status"
)

type fake struct{}

func (fake) Status() status.Status {
	return status.Status{View: "director", Cluster: &status.Cluster{Name: "prod", ID: "7f3c91a2"}}
}

func (fake) Admin(op string, names []string) ([]string, error) { return names, nil }

func (fake) SecretText(role string) (string, error) {
	if role == "director" {
		return "thelb1:director", nil
	}
	return "", errors.New("a secret's role is director or backend")
}

// runDir is a short path: a Unix socket's path is limited to about 100 bytes.
func runDir(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "thelb-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	return filepath.Join(base, "run")
}

func serve(t *testing.T, dir string, uid int) (*Server, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	s, err := Listen(dir, fake{}, logx.New(&logs, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	s.PeerUID = func(*net.UnixConn) (int, error) { return uid, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; s.Close() })
	return s, &logs
}

func TestStatusAndSecretForRoot(t *testing.T) {
	dir := runDir(t)
	s, logs := serve(t, dir, 0)
	resp, err := Call(dir, Request{Op: OpStatus})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status == nil || resp.Status.Cluster.Name != "prod" || resp.Status.Nodes == nil {
		t.Errorf("status %+v (tables must be lists, not null)", resp.Status)
	}
	resp, err = Call(dir, Request{Op: OpSecretShow, Role: "director"})
	if err != nil || resp.Secret != "thelb1:director" {
		t.Errorf("secret show: %q %v", resp.Secret, err)
	}
	if _, err := Call(dir, Request{Op: OpSecretShow, Role: "admin"}); err == nil {
		t.Error("a secret for an unknown role")
	}
	if _, err := Call(dir, Request{Op: "reboot"}); err == nil || !strings.Contains(err.Error(), "does not know") {
		t.Errorf("an unknown request: %v", err)
	}
	if strings.Contains(logs.String(), "thelb1:") {
		t.Errorf("a secret reached the log:\n%s", logs)
	}
	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the socket's mode is %04o, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Errorf("the run directory's mode is %04o, want 0700", di.Mode().Perm())
	}
}

func TestEveryoneButRootIsRefused(t *testing.T) {
	dir := runDir(t)
	_, logs := serve(t, dir, 1000)
	if _, err := Call(dir, Request{Op: OpStatus}); err == nil || !strings.Contains(err.Error(), "only root") {
		t.Errorf("a non-root caller: %v", err)
	}
	if !strings.Contains(logs.String(), "WARN request refused") {
		t.Errorf("the refusal was not logged:\n%s", logs)
	}
}

func TestOneTheLBPerNode(t *testing.T) {
	dir := runDir(t)
	serve(t, dir, 0)
	if _, err := Listen(dir, fake{}, logx.New(&bytes.Buffer{}, "socket")); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("a second Listen: %v, want ErrAlreadyRunning", err)
	}
}

func TestAStaleSocketIsReplaced(t *testing.T) {
	dir := runDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A socket nothing listens on, as a crash leaves it.
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, SocketName), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	_ = l.Close()
	if _, err := Call(dir, Request{Op: OpStatus}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("calling a stale socket: %v, want ErrNotRunning", err)
	}
	_, logs := serve(t, dir, 0)
	if !strings.Contains(logs.String(), "stale socket replaced") {
		t.Errorf("not logged:\n%s", logs)
	}
	if _, err := Call(dir, Request{Op: OpStatus}); err != nil {
		t.Errorf("after replacing it: %v", err)
	}
}

func TestCloseLeavesNothing(t *testing.T) {
	dir := runDir(t)
	s, err := Listen(dir, fake{}, logx.New(&bytes.Buffer{}, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s.Close() // twice is fine
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the run directory is still there: %v", err)
	}
	if _, err := Call(dir, Request{Op: OpStatus}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("after Close: %v, want ErrNotRunning", err)
	}
}

func TestVersionMismatch(t *testing.T) {
	dir := runDir(t)
	serve(t, dir, 0)
	c, err := net.Dial("unix", filepath.Join(dir, SocketName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(`{"v":2,"op":"status"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, _ := c.Read(buf)
	if !strings.Contains(string(buf[:n]), "version 1") {
		t.Errorf("answer %s", buf[:n])
	}
}
