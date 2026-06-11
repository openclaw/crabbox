package opencomputer

import "flag"

type openComputerFlagValues struct {
	APIURL          *string
	Workdir         *string
	CPU             *int
	MemoryMB        *int
	TimeoutSecs     *int
	ExecTimeoutSecs *int
	ForgetMissing   *bool
}

func RegisterOpenComputerProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return openComputerFlagValues{
		APIURL:          fs.String("opencomputer-api-url", defaults.OpenComputer.APIURL, "Trusted OpenComputer API base URL; not accepted from repository config"),
		Workdir:         fs.String("opencomputer-workdir", defaults.OpenComputer.Workdir, "Absolute working directory inside the sandbox (also used as sync target)"),
		CPU:             fs.Int("opencomputer-cpu", defaults.OpenComputer.CPU, "OpenComputer sandbox vCPU count (0 = service default; the service infers memory when omitted)"),
		MemoryMB:        fs.Int("opencomputer-memory-mb", defaults.OpenComputer.MemoryMB, "OpenComputer sandbox memory in MB (0 = service default; the service infers CPU when omitted)"),
		TimeoutSecs:     fs.Int("opencomputer-timeout-secs", defaults.OpenComputer.TimeoutSecs, "OpenComputer sandbox idle timeout in seconds (0 = service default)"),
		ExecTimeoutSecs: fs.Int("opencomputer-exec-timeout-secs", defaults.OpenComputer.ExecTimeoutSecs, "OpenComputer command timeout in seconds (0 = Crabbox default 3600)"),
		ForgetMissing:   fs.Bool("opencomputer-forget-missing", defaults.OpenComputer.ForgetMissing, "remove the local claim when stop gets 404 (explicit stale-claim cleanup)"),
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
	if flagWasSet(fs, "opencomputer-exec-timeout-secs") {
		cfg.OpenComputer.ExecTimeoutSecs = *v.ExecTimeoutSecs
	}
	if flagWasSet(fs, "opencomputer-forget-missing") {
		cfg.OpenComputer.ForgetMissing = *v.ForgetMissing
	}
	return nil
}
