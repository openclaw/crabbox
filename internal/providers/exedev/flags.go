package exedev

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterExeDevProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterExeDevConfigFlags(fs, defaults.ExeDev)
}

func ApplyExeDevProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatchesExact(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --exe-dev-cpus, --exe-dev-memory, and --exe-dev-disk", "use --exe-dev-image"); err != nil {
			return err
		}
	}
	v, ok := values.(core.ExeDevConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.ExeDev, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if err != nil {
		return err
	}
	if core.ProviderNameMatchesExact(cfg.Provider, Provider{}) {
		applyExeDevDefaults(cfg)
	}
	return nil
}
