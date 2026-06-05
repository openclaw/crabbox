package hyperv

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
	Switch *string
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Image:  fs.String("hyperv-image", defaults.HyperV.Image, "Windows VHDX template path for Hyper-V VM creation"),
		CPUs:   fs.Int("hyperv-cpu", defaults.HyperV.CPUs, "CPU count for Hyper-V leases"),
		Memory: fs.Int("hyperv-memory", defaults.HyperV.Memory, "memory in MB for Hyper-V leases"),
		Disk:   fs.Int("hyperv-disk", defaults.HyperV.Disk, "disk size in GB for Hyper-V leases"),
		Switch: fs.String("hyperv-switch", defaults.HyperV.Switch, "Hyper-V virtual switch name"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "hyperv-image") {
		cfg.HyperV.Image = *v.Image
	}
	if flagWasSet(fs, "hyperv-cpu") {
		cfg.HyperV.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "hyperv-memory") {
		cfg.HyperV.Memory = *v.Memory
	}
	if flagWasSet(fs, "hyperv-disk") {
		cfg.HyperV.Disk = *v.Disk
	}
	if flagWasSet(fs, "hyperv-switch") {
		cfg.HyperV.Switch = *v.Switch
	}
	if isHyperVProviderName(cfg.Provider) {
		applyDefaults(cfg)
	}
	return nil
}

func isHyperVProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "local-hyperv", "hyper-v", "windows-vm":
		return true
	default:
		return false
	}
}
