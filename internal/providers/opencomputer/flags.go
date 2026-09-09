package opencomputer

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterOpenComputerProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterOpenComputerConfigFlags(fs, defaults.OpenComputer)
}

func ApplyOpenComputerProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case providerName, "oc", "open-computer":
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --opencomputer-cpu and --opencomputer-memory-mb", "use --opencomputer-cpu and --opencomputer-memory-mb"); err != nil {
			return err
		}
	}
	v, ok := values.(core.OpenComputerConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.OpenComputer, fs)
	return nil
}
