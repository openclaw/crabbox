package multipass

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	CLIPath       *string
	Image         *string
	User          *string
	WorkRoot      *string
	CPUs          *int
	Memory        *string
	Disk          *string
	LaunchTimeout *string
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		CLIPath:       fs.String("multipass-cli", defaults.Multipass.CLIPath, "Multipass CLI path"),
		Image:         fs.String("multipass-image", defaults.Multipass.Image, "Multipass Ubuntu image selector"),
		User:          fs.String("multipass-user", defaults.Multipass.User, "SSH user created inside Multipass leases"),
		WorkRoot:      fs.String("multipass-work-root", defaults.Multipass.WorkRoot, "remote Crabbox work root inside Multipass leases"),
		CPUs:          fs.Int("multipass-cpus", defaults.Multipass.CPUs, "CPU count for Multipass leases; 0 leaves Multipass default"),
		Memory:        fs.String("multipass-memory", defaults.Multipass.Memory, "memory size for Multipass leases, for example 8G"),
		Disk:          fs.String("multipass-disk", defaults.Multipass.Disk, "disk size for Multipass leases, for example 30G"),
		LaunchTimeout: fs.String("multipass-launch-timeout", defaults.Multipass.LaunchTimeout.String(), "Multipass launch timeout"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "multipass-cli") {
		cfg.Multipass.CLIPath = *v.CLIPath
	}
	if flagWasSet(fs, "multipass-image") {
		cfg.Multipass.Image = *v.Image
		core.MarkMultipassImageExplicit(cfg)
	}
	if flagWasSet(fs, "multipass-user") {
		cfg.Multipass.User = *v.User
		cfg.SSHUser = *v.User
	}
	if flagWasSet(fs, "multipass-work-root") {
		cfg.Multipass.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "multipass-cpus") {
		cfg.Multipass.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "multipass-memory") {
		cfg.Multipass.Memory = *v.Memory
	}
	if flagWasSet(fs, "multipass-disk") {
		cfg.Multipass.Disk = *v.Disk
	}
	if flagWasSet(fs, "multipass-launch-timeout") {
		if err := core.ApplyLeaseDuration(&cfg.Multipass.LaunchTimeout, *v.LaunchTimeout); err != nil {
			return err
		}
	}
	if isMultipassProviderName(cfg.Provider) {
		applyDefaults(cfg)
	}
	return nil
}

func isMultipassProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "mp", "canonical-multipass":
		return true
	default:
		return false
	}
}
