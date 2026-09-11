package namespaceinstance

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterNamespaceInstanceConfigFlags(fs, defaults.NamespaceInstance)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.NamespaceInstanceConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.NamespaceInstance, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if err != nil {
		return err
	}
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		applyDefaults(cfg)
	}
	return nil
}
