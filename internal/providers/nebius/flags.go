package nebius

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

// RegisterNebiusProviderFlags exposes only non-secret Nebius settings.
// Authentication is owned by Nebius CLI profiles, not Crabbox argv.
func RegisterNebiusProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterNebiusConfigFlags(fs, defaults.Nebius)
}

func ApplyNebiusProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if ok, err := core.ApplyProviderConfigFlags[core.NebiusConfigFlagValues](cfg, fs, values, &cfg.Nebius, providerName); !ok || err != nil {
		return err
	}
	if cfg.Provider == providerName {
		return Provider{}.ValidateConfig(*cfg)
	}
	return nil
}
