//go:build profile

package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/pprof"
	"syscall"
	"time"
)

// A build for measuring only (go build -tags profile): SIGUSR1 writes a CPU
// profile of the next 30 seconds to /tmp/thelb-<pid>.cpu. Releases have none.
func init() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGUSR1)
	go func() {
		for range c {
			path := fmt.Sprintf("/tmp/thelb-%d.cpu", os.Getpid())
			f, err := os.Create(path)
			if err != nil {
				continue
			}
			if err := pprof.StartCPUProfile(f); err != nil {
				_ = f.Close()
				continue
			}
			time.Sleep(30 * time.Second)
			pprof.StopCPUProfile()
			_ = f.Close()
		}
	}()
}
