package tart

import (
	"flag"
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
		Disk:   fs.Int("tart-disk", defaults.Tart.Disk, "disk size in GB for tart VMs"),
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
		cfg.Tart.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "tart-memory") {
		cfg.Tart.Memory = *v.Memory
	}
	if flagWasSet(fs, "tart-disk") {
		cfg.Tart.Disk = *v.Disk
	}
	if isTartProviderName(cfg.Provider) {
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
