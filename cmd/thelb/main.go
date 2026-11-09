// Command thelb is theLB: L4 load balancing for Linux, in one binary.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/dmartsapp/thelb/internal/cli"
)

func main() {
	// SIGHUP stops theLB as SIGTERM does: it is what a closed terminal or SSH
	// session sends, and theLB reads no configuration to reload. Left to its
	// default, it ended the process at once, and the VIPs it held stayed on the
	// interface, answered for, with nothing behind them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	code := cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
