package azuredynamicsessions

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterAzureDynamicSessionsProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterAzureDynamicSessionsConfigFlags(fs, defaults.AzureDynamicSessions)
}

func ApplyAzureDynamicSessionsProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "choose pool sizing in Azure", "choose pool sizing in Azure"); err != nil {
			return err
		}
	}
	_, err := core.ApplyProviderConfigFlags[core.AzureDynamicSessionsConfigFlagValues](cfg, fs, values, &cfg.AzureDynamicSessions, providerName)
	return err
}
