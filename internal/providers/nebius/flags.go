package nebius

import core "github.com/openclaw/crabbox/internal/cli"

import "flag"

// RegisterNebiusProviderFlags exposes only non-secret Nebius settings.
// Authentication is owned by Nebius CLI profiles, not Crabbox argv.
func RegisterNebiusProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterNebiusConfigFlags(fs, defaults.Nebius)
}

func ApplyNebiusProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.NebiusConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Nebius, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if err != nil {
		return err
	}
	if cfg.Provider == providerName {
		return Provider{}.ValidateConfig(*cfg)
	}
	return nil
}
