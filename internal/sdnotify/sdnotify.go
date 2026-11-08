// Package sdnotify tells systemd how theLB is doing, for a unit with
// Type=notify and WatchdogSec=: ready, still alive, stopping. Outside systemd it
// does nothing.
package sdnotify

import (
	"net"
	"os"
	"strconv"
	"time"
)

// Notify sends state ("READY=1", "WATCHDOG=1", "STOPPING=1", ...) to systemd.
func Notify(state string) error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}
	if addr[0] == '@' { // an abstract socket
		addr = "\x00" + addr[1:]
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_, err = c.Write([]byte(state))
	return err
}

// WatchdogInterval is how often to tell systemd theLB is alive - half of
// WatchdogSec=, as systemd advises - or 0 when no watchdog is set for this
// process.
func WatchdogInterval() time.Duration {
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0
	}
	usec, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return 0
	}
	return time.Duration(usec) * time.Microsecond / 2
}
