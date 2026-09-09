package opencomputer

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterOpenComputerProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterOpenComputerConfigFlags(fs, defaults.OpenComputer)
}

func ApplyOpenComputerProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case providerName, "oc", "open-computer":
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=opencomputer; use --opencomputer-cpu and --opencomputer-memory-mb")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=opencomputer; use --opencomputer-cpu and --opencomputer-memory-mb")
		}
	}
	v, ok := values.(core.OpenComputerConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.OpenComputer, fs)
	return nil
}
