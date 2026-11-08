//go:build !linux

// Package dplane is the data plane of the director holding the service VIPs;
// directors run only on Linux.
package dplane

import "github.com/dmartsapp/thelb/internal/logx"

// Plane is not available here.
type Plane struct{}

// New is not available here.
func New(*logx.Logger) *Plane { return nil }
