package railway

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// RegisterRailwayProviderFlags exposes railway-specific flags. The API token is
// intentionally not surfaced as a flag because secrets must not be passed as
// command-line arguments; it is sourced from RAILWAY_API_TOKEN /
// CRABBOX_RAILWAY_API_TOKEN.
func RegisterRailwayProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterRailwayConfigFlags(fs, defaults.Railway)
}

func ApplyRailwayProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if isRailwayProviderName(cfg.Provider) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	v, ok := values.(core.RailwayConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.Railway, fs)
	return nil
}

func isRailwayProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "rail", "railwayapp":
		return true
	default:
		return false
	}
}
