package smolvm

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterSmolvmProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterSmolvmConfigFlags(fs, defaults.Smolvm)
}

func ApplySmolvmProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatchesExact(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --smolvm-cpus/--smolvm-memory-mb", "use --smolvm-image"); err != nil {
			return err
		}
	}
	v, ok := values.(core.SmolvmConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Smolvm, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if err != nil {
		return err
	}
	return validateConfig(*cfg)
}

func validateConfig(cfg core.Config) error {
	if network := strings.TrimSpace(cfg.Smolvm.Network); network != "" {
		switch strings.ToLower(network) {
		case "open", "blocked", "public", "private":
		default:
			return core.Exit(2, "invalid smolvm network %q (use open or blocked)", network)
		}
	}
	if cpus := cfg.Smolvm.CPUs; cpus < 0 {
		return core.Exit(2, "smolvm cpus must be >= 0")
	}
	if mem := cfg.Smolvm.MemoryMB; mem < 0 {
		return core.Exit(2, "smolvm memory-mb must be >= 0")
	}
	_, err := cleanWorkdir(workdir(cfg))
	return err
}
