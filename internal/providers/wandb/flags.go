package wandb

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// RegisterWandbProviderFlags exposes W&B sandbox flags. The API key is
// intentionally not surfaced as a flag because secrets must not be passed as
// command-line arguments; it is sourced from CRABBOX_WANDB_API_KEY,
// cfg.wandb.apiKey, or WANDB_API_KEY (in that precedence order).
func RegisterWandbProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterWandbConfigFlags(fs, defaults.Wandb)
}

func ApplyWandbProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	v, ok := values.(core.WandbConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Wandb, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	return err
}
