package opencomputer

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterOpenComputerProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterOpenComputerConfigFlags(fs, defaults.OpenComputer)
}

func ApplyOpenComputerProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --opencomputer-cpu and --opencomputer-memory-mb", "use --opencomputer-cpu and --opencomputer-memory-mb"); err != nil {
			return err
		}
	}
	_, err := core.ApplyProviderConfigFlags[core.OpenComputerConfigFlagValues](cfg, fs, values, &cfg.OpenComputer, providerName)
	return err
}
