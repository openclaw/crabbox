package nvidiabrev

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// RegisterNvidiaBrevProviderFlags exposes only non-secret Brev settings.
// Authentication is owned by the Brev CLI credential store, not Crabbox argv.
func RegisterNvidiaBrevProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterNvidiaBrevConfigFlags(fs, defaults.NvidiaBrev)
}

func ApplyNvidiaBrevProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --nvidia-brev-gpu-name", "use --nvidia-brev-type"); err != nil {
			return err
		}
	}
	v, ok := values.(core.NvidiaBrevConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.NvidiaBrev, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if applied.ReleaseAction {
		markReleaseActionExplicit(cfg)
	}
	if applied.WorkRoot {
		markNvidiaBrevWorkRootExplicit(cfg)
	}
	if err != nil {
		return err
	}
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		applyNvidiaBrevDefaults(cfg)
		return Provider{}.ValidateConfig(*cfg)
	}
	return nil
}
