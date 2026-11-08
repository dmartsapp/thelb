//go:build linux

package cli

import (
	"github.com/dmartsapp/thelb/internal/backend"
	"github.com/dmartsapp/thelb/internal/backnet"
	"github.com/dmartsapp/thelb/internal/director"
	"github.com/dmartsapp/thelb/internal/dplane"
	"github.com/dmartsapp/thelb/internal/logx"
)

func dataplane(log *logx.Logger) (director.Dataplane, error) { return dplane.New(log), nil }

func backendKernel(log *logx.Logger) (backend.Kernel, error) { return backnet.New(log), nil }
