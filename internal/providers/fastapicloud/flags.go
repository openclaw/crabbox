package fastapicloud

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// RegisterFastAPICloudProviderFlags exposes only non-secret provider flags.
// Deploy tokens are sourced from FASTAPI_CLOUD_TOKEN /
// CRABBOX_FASTAPI_CLOUD_TOKEN so they are not passed as command-line
// arguments.
func RegisterFastAPICloudProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterFastAPICloudConfigFlags(fs, defaults.FastAPICloud)
}

func ApplyFastAPICloudProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	_, err := core.ApplyProviderConfigFlags[core.FastAPICloudConfigFlagValues](cfg, fs, values, &cfg.FastAPICloud, providerName)
	return err
}
