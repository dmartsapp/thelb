//go:build !linux

package fingerprint

import "errors"

// ReadHost is Linux only: the fingerprint is made of Linux's machine ID.
func ReadHost(string) (Inputs, error) {
	return Inputs{}, errors.New("the node fingerprint is read from Linux's machine ID: theLB runs only on Linux")
}
