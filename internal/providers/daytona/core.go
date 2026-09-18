package daytona

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	targetLinux   = core.TargetLinux
	NetworkPublic = core.NetworkPublic
)

func validateNativeCredentialDestination(cfg core.Config) error {
	return core.ValidateNativeCredentialDestination(cfg, daytonaProvider)
}
