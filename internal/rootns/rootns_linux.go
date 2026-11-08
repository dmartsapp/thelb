//go:build linux

// Package rootns lets a test run as root in a network namespace of its own: the
// calling goroutine's thread leaves this machine's network, and whatever the
// test changes there goes away with it.
package rootns

import (
	"os"
	"runtime"
	"syscall"
	"testing"
)

// Enter puts the calling goroutine in a new network namespace, for the rest of
// the test, or skips the test when it is not root. The goroutine must make its
// netlink calls itself: other goroutines stay in this machine's network.
func Enter(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root, in a network namespace of its own: run with make e2e")
	}
	runtime.LockOSThread() // never unlocked: the thread exits with the goroutine
	if err := syscall.Unshare(syscall.CLONE_NEWNET); err != nil {
		t.Fatalf("a network namespace: %v", err)
	}
}
