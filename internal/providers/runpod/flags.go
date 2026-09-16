package runpod

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// RegisterRunpodProviderFlags exposes runpod-specific flags. The API key is
// intentionally not surfaced as a flag because secrets must not be passed as
// command-line arguments; it is sourced from RUNPOD_API_KEY /
// CRABBOX_RUNPOD_API_KEY.
func RegisterRunpodProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterRunpodConfigFlags(fs, defaults.Runpod)
}

func ApplyRunpodProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --runpod-instance-id", "use --runpod-image"); err != nil {
			return err
		}
	}
	v, ok := values.(core.RunpodConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Runpod, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if err != nil {
		return err
	}
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		applyRunpodDefaults(cfg)
	}
	return nil
}
