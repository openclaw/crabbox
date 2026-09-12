package orgo

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// RegisterOrgoProviderFlags exposes non-secret Orgo settings. The API key is
// intentionally not a flag; secrets are read from env/config only.
func RegisterOrgoProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterOrgoConfigFlags(fs, defaults.Orgo)
}

func ApplyOrgoProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	v, ok := values.(core.OrgoConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Orgo, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	return err
}
