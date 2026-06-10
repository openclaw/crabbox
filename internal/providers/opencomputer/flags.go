package opencomputer

import "flag"

type openComputerFlagValues struct {
	APIURL      *string
	Workdir     *string
	CPU         *int
	MemoryMB    *int
	TimeoutSecs *int
}

func RegisterOpenComputerProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return openComputerFlagValues{
		APIURL:      fs.String("opencomputer-api-url", defaults.OpenComputer.APIURL, "OpenComputer API base URL"),
		Workdir:     fs.String("opencomputer-workdir", defaults.OpenComputer.Workdir, "Absolute working directory inside the sandbox (also used as sync target)"),
		CPU:         fs.Int("opencomputer-cpu", defaults.OpenComputer.CPU, "OpenComputer sandbox vCPU count (0 = service default; CPU+memory must form an allowed tier)"),
		MemoryMB:    fs.Int("opencomputer-memory-mb", defaults.OpenComputer.MemoryMB, "OpenComputer sandbox memory in MB (0 = service default; CPU+memory must form an allowed tier)"),
		TimeoutSecs: fs.Int("opencomputer-timeout-secs", defaults.OpenComputer.TimeoutSecs, "OpenComputer sandbox idle timeout in seconds (0 = service default)"),
	}
}

func ApplyOpenComputerProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(openComputerFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "opencomputer-api-url") {
		cfg.OpenComputer.APIURL = *v.APIURL
	}
	if flagWasSet(fs, "opencomputer-workdir") {
		cfg.OpenComputer.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "opencomputer-cpu") {
		cfg.OpenComputer.CPU = *v.CPU
	}
	if flagWasSet(fs, "opencomputer-memory-mb") {
		cfg.OpenComputer.MemoryMB = *v.MemoryMB
	}
	if flagWasSet(fs, "opencomputer-timeout-secs") {
		cfg.OpenComputer.TimeoutSecs = *v.TimeoutSecs
	}
	return nil
}
