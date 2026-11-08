//go:build !linux

package localsock

import (
	"errors"
	"net"
)

// peerUID is Linux only: elsewhere every caller is refused.
func peerUID(*net.UnixConn) (int, error) {
	return -1, errors.New("the caller's identity is read only on Linux")
}
