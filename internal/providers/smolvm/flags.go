package smolvm

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterSmolvmProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterSmolvmConfigFlags(fs, defaults.Smolvm)
}

func ApplySmolvmProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName || cfg.Provider == "smol" || cfg.Provider == "smolmachines" || cfg.Provider == "smolfleet" {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s; use --smolvm-cpus/--smolvm-memory-mb", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s; use --smolvm-image", providerName)
		}
	}
	v, ok := values.(core.SmolvmConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.Smolvm, fs)
	return validateConfig(*cfg)
}

func validateConfig(cfg Config) error {
	if network := strings.TrimSpace(cfg.Smolvm.Network); network != "" {
		switch strings.ToLower(network) {
		case "open", "blocked", "public", "private":
		default:
			return exit(2, "invalid smolvm network %q (use open or blocked)", network)
		}
	}
	if cpus := cfg.Smolvm.CPUs; cpus < 0 {
		return exit(2, "smolvm cpus must be >= 0")
	}
	if mem := cfg.Smolvm.MemoryMB; mem < 0 {
		return exit(2, "smolvm memory-mb must be >= 0")
	}
	_, err := cleanWorkdir(workdir(cfg))
	return err
}
