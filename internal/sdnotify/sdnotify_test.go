package sdnotify

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestNotifyOutsideSystemdDoesNothing(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := Notify("READY=1"); err != nil {
		t.Errorf("without systemd: %v", err)
	}
}

func TestNotifyReachesSystemdsSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "thelb-sd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "notify")
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	t.Setenv("NOTIFY_SOCKET", path)
	if err := Notify("READY=1\nSTATUS=ready"); err != nil {
		t.Fatal(err)
	}
	_ = l.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 256)
	n, err := l.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "READY=1\nSTATUS=ready" {
		t.Errorf("systemd got %q", buf[:n])
	}
}

func TestWatchdogInterval(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "30000000")
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))
	if got := WatchdogInterval(); got != 15*time.Second {
		t.Errorf("WatchdogSec=30: %v, want 15s", got)
	}
	t.Setenv("WATCHDOG_PID", "1")
	if got := WatchdogInterval(); got != 0 {
		t.Errorf("another process's watchdog: %v", got)
	}
	t.Setenv("WATCHDOG_PID", "")
	t.Setenv("WATCHDOG_USEC", "")
	if got := WatchdogInterval(); got != 0 {
		t.Errorf("no watchdog: %v", got)
	}
}
