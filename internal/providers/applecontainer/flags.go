package applecontainer

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	CLIPath  *string
	Image    *string
	User     *string
	WorkRoot *string
	CPUs     *int
	Memory   *string
	ExtraRun *string
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		CLIPath:  fs.String("apple-container-cli", defaults.AppleContainer.CLIPath, "path to Apple's container CLI"),
		Image:    fs.String("apple-container-image", defaults.AppleContainer.Image, "container image for apple-container leases"),
		User:     fs.String("apple-container-user", defaults.AppleContainer.User, "SSH user created inside apple-container leases"),
		WorkRoot: fs.String("apple-container-work-root", defaults.AppleContainer.WorkRoot, "remote Crabbox work root inside apple-container leases"),
		CPUs:     fs.Int("apple-container-cpus", defaults.AppleContainer.CPUs, "CPU limit for apple-container leases; 0 leaves runtime default"),
		Memory:   fs.String("apple-container-memory", defaults.AppleContainer.Memory, "memory limit for apple-container leases, for example 8g"),
		ExtraRun: fs.String("apple-container-extra-run-args", strings.Join(defaults.AppleContainer.ExtraRunArgs, " "), "extra arguments appended to container run, space separated"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "apple-container-cli") {
		cfg.AppleContainer.CLIPath = *v.CLIPath
	}
	if core.FlagWasSet(fs, "apple-container-image") {
		cfg.AppleContainer.Image = *v.Image
		core.MarkAppleContainerImageExplicit(cfg)
	}
	if core.FlagWasSet(fs, "apple-container-user") {
		cfg.AppleContainer.User = *v.User
		cfg.SSHUser = *v.User
	}
	if core.FlagWasSet(fs, "apple-container-work-root") {
		cfg.AppleContainer.WorkRoot = *v.WorkRoot
		cfg.WorkRoot = *v.WorkRoot
	}
	if core.FlagWasSet(fs, "apple-container-cpus") {
		cfg.AppleContainer.CPUs = *v.CPUs
	}
	if core.FlagWasSet(fs, "apple-container-memory") {
		cfg.AppleContainer.Memory = *v.Memory
	}
	if core.FlagWasSet(fs, "apple-container-extra-run-args") {
		cfg.AppleContainer.ExtraRunArgs = splitExtraArgs(*v.ExtraRun)
	}
	if isAppleContainerProvider(cfg.Provider) {
		applyDefaults(cfg)
	}
	return nil
}

func splitExtraArgs(value string) []string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return nil
	}
	return fields
}
