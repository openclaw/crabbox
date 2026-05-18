package exedev

import "flag"

type exeDevFlagValues struct {
	ControlHost *string
	Image       *string
	CPUs        *int
	Memory      *string
	Disk        *string
	Command     *string
	User        *string
	WorkRoot    *string
	NoEmail     *bool
}

func RegisterExeDevProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return exeDevFlagValues{
		ControlHost: fs.String("exe-dev-control-host", defaults.ExeDev.ControlHost, "exe.dev SSH API host"),
		Image:       fs.String("exe-dev-image", defaults.ExeDev.Image, "exe.dev VM image"),
		CPUs:        fs.Int("exe-dev-cpus", defaults.ExeDev.CPUs, "exe.dev VM CPUs"),
		Memory:      fs.String("exe-dev-memory", defaults.ExeDev.Memory, "exe.dev VM memory, for example 4GB"),
		Disk:        fs.String("exe-dev-disk", defaults.ExeDev.Disk, "exe.dev VM disk, for example 10GB"),
		Command:     fs.String("exe-dev-command", defaults.ExeDev.Command, "exe.dev container command"),
		User:        fs.String("exe-dev-user", defaults.ExeDev.User, "SSH user for exe.dev VMs"),
		WorkRoot:    fs.String("exe-dev-work-root", defaults.ExeDev.WorkRoot, "remote Crabbox work root on exe.dev VMs"),
		NoEmail:     fs.Bool("exe-dev-no-email", defaults.ExeDev.NoEmail, "suppress exe.dev VM notification email"),
	}
}

func ApplyExeDevProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName || cfg.Provider == "exe" || cfg.Provider == "exedev" {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s; use --exe-dev-cpus, --exe-dev-memory, and --exe-dev-disk", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s; use --exe-dev-image", providerName)
		}
	}
	v, ok := values.(exeDevFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "exe-dev-control-host") {
		cfg.ExeDev.ControlHost = *v.ControlHost
	}
	if flagWasSet(fs, "exe-dev-image") {
		cfg.ExeDev.Image = *v.Image
	}
	if flagWasSet(fs, "exe-dev-cpus") {
		cfg.ExeDev.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "exe-dev-memory") {
		cfg.ExeDev.Memory = *v.Memory
	}
	if flagWasSet(fs, "exe-dev-disk") {
		cfg.ExeDev.Disk = *v.Disk
	}
	if flagWasSet(fs, "exe-dev-command") {
		cfg.ExeDev.Command = *v.Command
	}
	if flagWasSet(fs, "exe-dev-user") {
		cfg.ExeDev.User = *v.User
	}
	if flagWasSet(fs, "exe-dev-work-root") {
		cfg.ExeDev.WorkRoot = *v.WorkRoot
	}
	if flagWasSet(fs, "exe-dev-no-email") {
		cfg.ExeDev.NoEmail = *v.NoEmail
	}
	if cfg.Provider == providerName || cfg.Provider == "exe" || cfg.Provider == "exedev" {
		applyExeDevDefaults(cfg)
	}
	return nil
}
