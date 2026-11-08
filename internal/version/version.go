// Package version says which theLB this is.
package version

import (
	"fmt"
	"runtime"
)

// Version and Commit are set when a release is built:
//
//	-ldflags "-X github.com/dmartsapp/thelb/internal/version.Version=0.1.0 -X github.com/dmartsapp/thelb/internal/version.Commit=<sha>"
var (
	Version = "0.1.0-dev"
	Commit  = "unknown"
)

// String is what `thelb --version` prints: the version, the commit, and the Go
// toolchain and platform the binary was built with.
func String() string {
	return fmt.Sprintf("thelb %s (commit %s, %s %s/%s)", Version, Commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
