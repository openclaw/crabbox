package vast

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// RegisterVastProviderFlags exposes only non-secret Vast settings. API keys
// are sourced from CRABBOX_VAST_API_KEY / VAST_API_KEY and never argv.
func RegisterVastProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterVastConfigFlags(fs, defaults.Vast)
}

func ApplyVastProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --vast-gpu-name or --vast-gpu-count", "use --vast-image"); err != nil {
			return err
		}
	}
	v, ok := values.(core.VastConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Vast, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if applied.InstanceType {
		cfg.Vast.InstanceType = normalizeInstanceType(cfg.Vast.InstanceType)
	}
	if applied.WorkRoot {
		markVastWorkRootExplicit(cfg)
	}
	if applied.ReleaseAction {
		markReleaseActionExplicit(cfg)
	}
	if err != nil {
		return err
	}
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		return Provider{}.ValidateConfig(*cfg)
	}
	return nil
}
