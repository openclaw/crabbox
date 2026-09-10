package orgo

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// RegisterOrgoProviderFlags exposes non-secret Orgo settings. The API key is
// intentionally not a flag; secrets are read from env/config only.
func RegisterOrgoProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterOrgoConfigFlags(fs, defaults.Orgo)
}

func ApplyOrgoProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	v, ok := values.(core.OrgoConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.Orgo, fs)
	return nil
}
