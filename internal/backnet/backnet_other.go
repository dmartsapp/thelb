//go:build !linux

// Package backnet is a backend's kernel state; backends run only on Linux.
package backnet

import "github.com/dmartsapp/thelb/internal/logx"

// Host is not available here.
type Host struct{}

// New is not available here.
func New(*logx.Logger) *Host { return nil }
