package unikraftcloud

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// registerUnikraftCloudProviderFlags exposes only non-secret provider flags.
func registerUnikraftCloudProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterUnikraftCloudConfigFlags(fs, defaults.UnikraftCloud)
}

func applyUnikraftCloudProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	_, err := core.ApplyProviderConfigFlags[core.UnikraftCloudConfigFlagValues](cfg, fs, values, &cfg.UnikraftCloud, "unikraft-cloud")
	return err
}
