package tart

import (
	"flag"
	"os"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	Image  *string
	User   *string
	CPUs   *int
	Memory *int
	Disk   *int
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Image:  fs.String("tart-image", defaults.Tart.Image, "tart base image to clone from"),
		User:   fs.String("tart-user", defaults.Tart.User, "guest user account for SSH and desktop/VNC"),
		CPUs:   fs.Int("tart-cpu", defaults.Tart.CPUs, "CPU count for tart VMs"),
		Memory: fs.Int("tart-memory", defaults.Tart.Memory, "memory in MB for tart VMs"),
		Disk:   fs.Int("tart-disk", defaults.Tart.Disk, "disk size in GB for tart VMs (0 = use clone default)"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "tart-image") {
		cfg.Tart.Image = *v.Image
		core.MarkTartImageExplicit(cfg)
	}
	if flagWasSet(fs, "tart-user") {
		cfg.Tart.User = *v.User
	}
	if flagWasSet(fs, "tart-cpu") {
		if *v.CPUs < 4 {
			return exit(2, "--tart-cpu must be at least 4 (got %d)", *v.CPUs)
		}
		cfg.Tart.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "tart-memory") {
		if *v.Memory < 4096 {
			return exit(2, "--tart-memory must be at least 4096 MB (got %d)", *v.Memory)
		}
		cfg.Tart.Memory = *v.Memory
	}
	if flagWasSet(fs, "tart-disk") {
		if *v.Disk < 0 {
			return exit(2, "--tart-disk must be non-negative (got %d)", *v.Disk)
		}
		cfg.Tart.Disk = *v.Disk
		if *v.Disk > 0 {
			core.MarkTartDiskExplicit(cfg)
		}
	}
	if isTartProviderName(cfg.Provider) {
		if core.IsTargetExplicit(cfg) && cfg.TargetOS != targetMacOS {
			return exit(2, "provider=%s supports target=%s only (got %s)", providerName, targetMacOS, cfg.TargetOS)
		}
		if !core.IsTargetExplicit(cfg) && cfg.TargetOS == "linux" {
			cfg.TargetOS = targetMacOS
		}
		if cfg.Tart.CPUs < 0 || (cfg.Tart.CPUs > 0 && cfg.Tart.CPUs < 4) || (cfg.Tart.CPUs == 0 && core.IsTartCPUsExplicit(cfg)) {
			return exit(2, "tart cpu count must be at least 4 (got %d)", cfg.Tart.CPUs)
		}
		if cfg.Tart.Memory < 0 || (cfg.Tart.Memory > 0 && cfg.Tart.Memory < 4096) || (cfg.Tart.Memory == 0 && core.IsTartMemoryExplicit(cfg)) {
			return exit(2, "tart memory must be at least 4096 MB (got %d)", cfg.Tart.Memory)
		}
		if cfg.Tart.Disk < 0 {
			return exit(2, "tart disk size must be non-negative (got %d)", cfg.Tart.Disk)
		}
		if err := validateTartEnvInt("CRABBOX_TART_CPUS", 4, "tart cpu count must be at least 4"); err != nil {
			return err
		}
		if err := validateTartEnvInt("CRABBOX_TART_MEMORY", 4096, "tart memory must be at least 4096 MB"); err != nil {
			return err
		}
		if err := validateTartEnvIntPositive("CRABBOX_TART_DISK", "tart disk size must be a positive integer"); err != nil {
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
		return exit(2, "%s must be a valid integer (got %q)", name, v)
	}
	if n < floor {
		return exit(2, "%s (got %d)", floorMsg, n)
	}
	return nil
}

func validateTartEnvIntPositive(name string, msg string) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return exit(2, "%s must be a valid integer (got %q)", name, v)
	}
	if n <= 0 {
		return exit(2, "%s (got %d)", msg, n)
	}
	return nil
}

func isTartProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "local-tart", "macos-vm":
		return true
	default:
		return false
	}
}
