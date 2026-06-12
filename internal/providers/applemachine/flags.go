package applemachine

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	CLIPath *string
	Image   *string
	CPUs    *int
	Memory  *string
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		CLIPath: fs.String("apple-machine-cli", defaults.AppleContainer.CLIPath, "path to Apple's container CLI"),
		Image:   fs.String("apple-machine-image", defaults.AppleContainer.Image, "OCI image for apple-machine leases"),
		CPUs:    fs.Int("apple-machine-cpus", defaults.AppleContainer.CPUs, "CPU count for apple-machine leases"),
		Memory:  fs.String("apple-machine-memory", defaults.AppleContainer.Memory, "memory for apple-machine leases, for example 8G"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if core.FlagWasSet(fs, "apple-machine-cli") {
		cfg.AppleContainer.CLIPath = *v.CLIPath
	}
	if core.FlagWasSet(fs, "apple-machine-image") {
		cfg.AppleContainer.Image = *v.Image
		core.MarkAppleContainerImageExplicit(cfg)
	}
	if core.FlagWasSet(fs, "apple-machine-cpus") {
		cfg.AppleContainer.CPUs = *v.CPUs
	}
	if core.FlagWasSet(fs, "apple-machine-memory") {
		cfg.AppleContainer.Memory = *v.Memory
	}
	return nil
}
