package tenki

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	tenkiProvider = "tenki"
	targetLinux   = core.TargetLinux
	networkPublic = core.NetworkPublic
)

func validateNativeCredentialDestination(cfg core.Config) error {
	return core.ValidateNativeCredentialDestination(cfg, tenkiProvider)
}
