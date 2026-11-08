//go:build !linux

package cli

import (
	"errors"

	"github.com/dmartsapp/thelb/internal/backend"
	"github.com/dmartsapp/thelb/internal/director"
	"github.com/dmartsapp/thelb/internal/logx"
)

func dataplane(*logx.Logger) (director.Dataplane, error) {
	return nil, errors.New("a director's data plane is Linux only")
}

func backendKernel(*logx.Logger) (backend.Kernel, error) {
	return nil, errors.New("a backend's kernel state is Linux only")
}
