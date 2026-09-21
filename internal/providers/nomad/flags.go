package nomad

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterNomadProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterNomadConfigFlags(fs, defaults.Nomad)
}

func ApplyNomadProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if core.FlagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=nomad; use --nomad-cpu/--nomad-memory-mb")
		}
		if core.FlagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=nomad; use --nomad-image")
		}
	}
	if ok, err := core.ApplyProviderConfigFlags[core.NomadConfigFlagValues](cfg, fs, values, &cfg.Nomad, providerName); !ok || err != nil {
		return err
	}
	return validateConfig(*cfg)
}
