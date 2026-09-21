package hyperv

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterHyperVConfigFlags(fs, defaults.HyperV)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if ok, err := core.ApplyProviderConfigFlags[core.HyperVConfigFlagValues](cfg, fs, values, &cfg.HyperV, providerName); !ok || err != nil {
		return err
	}
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		// Target flags are applied after provider flags on several lifecycle
		// commands. Leave target validation to the centralized provider-target
		// check after all flag sources have been applied. When no target source
		// is explicit, adopt the provider's Windows default here.
		if !core.IsTargetExplicit(cfg) && !core.FlagWasSet(fs, "target") {
			cfg.TargetOS = targetWindows
		}
		applyDefaults(cfg)
	}
	return nil
}
