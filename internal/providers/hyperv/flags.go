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
		if !core.IsTargetExplicit(cfg) && !core.FlagWasSet(fs, "target") {
			cfg.TargetOS = targetWindows
		}
		applyDefaults(cfg)
	}
	return nil
}
