//go:build !linux

package firecracker

import (
	"context"

	core "github.com/openclaw/crabbox/internal/cli"
)

type sdkMachineFactory struct {
	LogWriter any
}

func (sdkMachineFactory) New(_ context.Context, _ machineLaunchConfig) (machine, error) {
	return nil, core.Exit(2, "provider=firecracker requires a Linux KVM host")
}
