package opencomputer

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterOpenComputerProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterOpenComputerConfigFlags(fs, defaults.OpenComputer)
}

func ApplyOpenComputerProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --opencomputer-cpu and --opencomputer-memory-mb", "use --opencomputer-cpu and --opencomputer-memory-mb"); err != nil {
			return err
		}
	}
	v, ok := values.(core.OpenComputerConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.OpenComputer, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	return err
}
