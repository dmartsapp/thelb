//go:build !linux

package nft

import "errors"

// Apply is Linux only.
func Apply([]Map, ...Clamp) error { return errors.New("nftables is Linux only") }

// Remove is Linux only.
func Remove() error { return errors.New("nftables is Linux only") }

// Exists is Linux only.
func Exists() (bool, error) { return false, errors.New("nftables is Linux only") }
