package namespaceinstance

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterNamespaceInstanceConfigFlags(fs, defaults.NamespaceInstance)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if ok, err := core.ApplyProviderConfigFlags[core.NamespaceInstanceConfigFlagValues](cfg, fs, values, &cfg.NamespaceInstance, providerName); !ok || err != nil {
		return err
	}
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		applyDefaults(cfg)
	}
	return nil
}
