package railway

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// RegisterRailwayProviderFlags exposes railway-specific flags. The API token is
// intentionally not surfaced as a flag because secrets must not be passed as
// command-line arguments; it is sourced from RAILWAY_API_TOKEN /
// CRABBOX_RAILWAY_API_TOKEN.
func RegisterRailwayProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterRailwayConfigFlags(fs, defaults.Railway)
}

func ApplyRailwayProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	_, err := core.ApplyProviderConfigFlags[core.RailwayConfigFlagValues](cfg, fs, values, &cfg.Railway, providerName)
	return err
}
