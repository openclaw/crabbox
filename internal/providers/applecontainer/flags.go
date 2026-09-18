package applecontainer

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) ApplyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.AppleContainerConfigFlagValues)
	if !ok {
		return nil
	}
	applied := v.Apply(&cfg.AppleContainer, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, "apple-container", "apple-machine")
	if applied.Image {
		core.MarkAppleContainerImageExplicit(cfg)
	}
	if applied.User {
		cfg.SSHUser = cfg.AppleContainer.User
	}
	if applied.WorkRoot {
		cfg.WorkRoot = cfg.AppleContainer.WorkRoot
	}
	if core.ProviderNameMatchesExact(cfg.Provider, Provider{}) {
		applyDefaults(cfg)
	}
	return nil
}
