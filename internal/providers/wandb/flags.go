package wandb

import (
	"flag"
	"strings"
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
	if isWandbProviderName(cfg.Provider) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s", providerName)
		}
	}
	v, ok := values.(wandbFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "wandb-image") {
		cfg.Wandb.DefaultImage = *v.DefaultImage
	}
	if flagWasSet(fs, "wandb-max-lifetime") {
		cfg.Wandb.MaxLifetimeSeconds = *v.MaxLifetimeSeconds
	}
	return nil
}

// isWandbProviderName is consulted from every routing switch (provider_backend,
// flags, run, stop) so aliases share a single source of truth.
func isWandbProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "weights-and-biases":
		return true
	default:
		return false
	}
}
