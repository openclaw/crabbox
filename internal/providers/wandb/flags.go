package wandb

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

type wandbFlagValues struct {
	DefaultImage       *string
	MaxLifetimeSeconds *int
}

// RegisterWandbProviderFlags exposes W&B sandbox flags. The API key is
// intentionally not surfaced as a flag because secrets must not be passed as
// command-line arguments; it is sourced from CRABBOX_WANDB_API_KEY,
// cfg.wandb.apiKey, or WANDB_API_KEY (in that precedence order).
func RegisterWandbProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return wandbFlagValues{
		DefaultImage:       fs.String("wandb-image", defaults.Wandb.DefaultImage, "Container image used when acquiring a new W&B sandbox"),
		MaxLifetimeSeconds: fs.Int("wandb-max-lifetime", defaults.Wandb.MaxLifetimeSeconds, "Maximum sandbox lifetime in seconds before W&B reclaims it"),
	}
}

func ApplyWandbProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	v, ok := values.(wandbFlagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "wandb-image") {
		cfg.Wandb.DefaultImage = *v.DefaultImage
	}
	if core.FlagWasSet(fs, "wandb-max-lifetime") {
		cfg.Wandb.MaxLifetimeSeconds = *v.MaxLifetimeSeconds
	}
	return nil
}
