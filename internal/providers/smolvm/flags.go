package smolvm

import (
	"flag"
	"strings"
)

type flagValues struct {
	BaseURL  *string
	Image    *string
	Workdir  *string
	CPUs     *int
	MemoryMB *int
	Network  *string
	Keep     *bool
}

func RegisterSmolvmProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return flagValues{
		BaseURL:  fs.String("smolvm-base-url", defaults.Smolvm.BaseURL, "SmolVM / SmolFleet API base URL"),
		Image:    fs.String("smolvm-image", defaults.Smolvm.Image, "source image for smolvm machines (e.g. ubuntu:24.04)"),
		Workdir:  fs.String("smolvm-workdir", defaults.Smolvm.Workdir, "absolute working directory inside the smolvm machine"),
		CPUs:     fs.Int("smolvm-cpus", defaults.Smolvm.CPUs, "number of vCPUs for the smolvm machine"),
		MemoryMB: fs.Int("smolvm-memory-mb", defaults.Smolvm.MemoryMB, "memory in MiB for the smolvm machine"),
		Network:  fs.String("smolvm-network", defaults.Smolvm.Network, "network mode: open or blocked"),
		Keep:     fs.Bool("smolvm-keep", defaults.Smolvm.Keep, "keep the smolvm machine after run (do not auto-delete)"),
	}
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
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "smolvm-base-url") {
		cfg.Smolvm.BaseURL = *v.BaseURL
	}
	if flagWasSet(fs, "smolvm-image") {
		cfg.Smolvm.Image = *v.Image
	}
	if flagWasSet(fs, "smolvm-workdir") {
		cfg.Smolvm.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "smolvm-cpus") {
		cfg.Smolvm.CPUs = *v.CPUs
	}
	if flagWasSet(fs, "smolvm-memory-mb") {
		cfg.Smolvm.MemoryMB = *v.MemoryMB
	}
	if flagWasSet(fs, "smolvm-network") {
		cfg.Smolvm.Network = *v.Network
	}
	if flagWasSet(fs, "smolvm-keep") {
		cfg.Smolvm.Keep = *v.Keep
	}
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
