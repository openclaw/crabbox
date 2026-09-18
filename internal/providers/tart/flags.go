package tart

import (
	"flag"
	"os"
	"strconv"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterTartConfigFlags(fs, defaults.Tart)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.TartConfigFlagValues)
	if !ok {
		return nil
	}
	visited := core.TartConfigFlagPresence(fs)
	if visited.Image {
		cfg.Tart.Image = *v.Image
		core.MarkTartImageExplicit(cfg)
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.User {
		cfg.Tart.User = *v.User
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.CPUs {
		if *v.CPUs < 4 {
			return core.Exit(2, "--tart-cpu must be at least 4 (got %d)", *v.CPUs)
		}
		cfg.Tart.CPUs = *v.CPUs
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.Memory {
		if *v.Memory < 4096 {
			return core.Exit(2, "--tart-memory must be at least 4096 MB (got %d)", *v.Memory)
		}
		cfg.Tart.Memory = *v.Memory
		core.RecordProviderFlagInputs(cfg, true, providerName)
	}
	if visited.Disk {
		if *v.Disk < 0 {
			return core.Exit(2, "--tart-disk must be non-negative (got %d)", *v.Disk)
		}
		cfg.Tart.Disk = *v.Disk
		core.RecordProviderFlagInputs(cfg, true, providerName)
		if *v.Disk > 0 {
			core.MarkTartDiskExplicit(cfg)
		}
	}
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		if core.IsTargetExplicit(cfg) && cfg.TargetOS != targetMacOS {
			return core.Exit(2, "provider=%s supports target=%s only (got %s)", providerName, targetMacOS, cfg.TargetOS)
		}
		if !core.IsTargetExplicit(cfg) && cfg.TargetOS == "linux" {
			cfg.TargetOS = targetMacOS
		}
		if cfg.Tart.CPUs < 0 || (cfg.Tart.CPUs > 0 && cfg.Tart.CPUs < 4) || (cfg.Tart.CPUs == 0 && core.IsTartCPUsExplicit(cfg)) {
			return core.Exit(2, "tart cpu count must be at least 4 (got %d)", cfg.Tart.CPUs)
		}
		if cfg.Tart.Memory < 0 || (cfg.Tart.Memory > 0 && cfg.Tart.Memory < 4096) || (cfg.Tart.Memory == 0 && core.IsTartMemoryExplicit(cfg)) {
			return core.Exit(2, "tart memory must be at least 4096 MB (got %d)", cfg.Tart.Memory)
		}
		if cfg.Tart.Disk < 0 {
			return core.Exit(2, "tart disk size must be non-negative (got %d)", cfg.Tart.Disk)
		}
		if err := validateTartEnvInt("CRABBOX_TART_CPUS", 4, "tart cpu count must be at least 4"); err != nil {
			return err
		}
		if err := validateTartEnvInt("CRABBOX_TART_MEMORY", 4096, "tart memory must be at least 4096 MB"); err != nil {
			return err
		}
		if err := validateTartEnvInt("CRABBOX_TART_DISK", 0, "tart disk size must be non-negative"); err != nil {
			return err
		}
		applyDefaults(cfg)
	}
	return nil
}

func validateTartEnvInt(name string, floor int, floorMsg string) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return core.Exit(2, "%s must be a valid integer (got %q)", name, v)
	}
	if n < floor {
		return core.Exit(2, "%s (got %d)", floorMsg, n)
	}
	return nil
}
