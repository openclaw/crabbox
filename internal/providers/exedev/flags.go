package exedev

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterExeDevProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterExeDevConfigFlags(fs, defaults.ExeDev)
}

func ApplyExeDevProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if core.ProviderNameMatchesExact(cfg.Provider, Provider{}) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --exe-dev-cpus, --exe-dev-memory, and --exe-dev-disk", "use --exe-dev-image"); err != nil {
			return err
		}
	}
	v, ok := values.(core.ExeDevConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.ExeDev, fs)
	if core.ProviderNameMatchesExact(cfg.Provider, Provider{}) {
		applyExeDevDefaults(cfg)
	}
	return nil
}
