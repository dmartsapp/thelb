//go:build !linux

package preflight

import "net/netip"

// Director: theLB runs only on Linux.
func Director(netip.Addr) (string, error) { return "", Error{{"Linux", "theLB runs only on Linux"}} }

// Backend: theLB runs only on Linux.
func Backend(netip.Addr) (string, error) { return "", Error{{"Linux", "theLB runs only on Linux"}} }
