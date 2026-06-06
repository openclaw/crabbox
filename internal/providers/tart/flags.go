package tart

import (
	"flag"
	"os"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	Image  *string
	CPUs   *int
	Memory *int
	Disk   *int
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Image:  fs.String("tart-image", defaults.Tart.Image, "tart base image to clone from"),
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
		if *v.Disk <= 0 {
			return exit(2, "--tart-disk must be a positive integer (got %d)", *v.Disk)
		}
		cfg.Tart.Disk = *v.Disk
		core.MarkTartDiskExplicit(cfg)
	}
	if isTartProviderName(cfg.Provider) {
		if core.IsTargetExplicit(cfg) && cfg.TargetOS != targetMacOS {
			return exit(2, "provider=%s supports target=%s only (got %s)", providerName, targetMacOS, cfg.TargetOS)
		}
		if !core.IsTargetExplicit(cfg) && cfg.TargetOS == "linux" {
			cfg.TargetOS = targetMacOS
		}
		if cfg.Tart.CPUs < 0 || (cfg.Tart.CPUs > 0 && cfg.Tart.CPUs < 4) {
			return exit(2, "tart cpu count must be at least 4 (got %d)", cfg.Tart.CPUs)
		}
		if cfg.Tart.Memory < 0 || (cfg.Tart.Memory > 0 && cfg.Tart.Memory < 4096) {
			return exit(2, "tart memory must be at least 4096 MB (got %d)", cfg.Tart.Memory)
		}
		if cfg.Tart.Disk < 0 {
			return exit(2, "tart disk size must be positive (got %d)", cfg.Tart.Disk)
		}
		if os.Getenv("CRABBOX_TART_CPUS") == "0" {
			return exit(2, "CRABBOX_TART_CPUS must be at least 4 (got 0)")
		}
		if os.Getenv("CRABBOX_TART_MEMORY") == "0" {
			return exit(2, "CRABBOX_TART_MEMORY must be at least 4096 MB (got 0)")
		}
		if os.Getenv("CRABBOX_TART_DISK") == "0" {
			return exit(2, "CRABBOX_TART_DISK must be a positive integer (got 0)")
		}
		applyDefaults(cfg)
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
