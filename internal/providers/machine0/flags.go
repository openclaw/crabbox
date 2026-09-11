package machine0

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterMachine0ConfigFlags(fs, defaults.Machine0)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.Machine0ConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Machine0, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if applied.Size {
		cfg.Machine0.SizeExplicit = true
		cfg.ServerType = cfg.Machine0.Size
		cfg.ServerTypeExplicit = true
	}
	if applied.WorkRoot {
		cfg.WorkRoot = cfg.Machine0.WorkRoot
	}
	if err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		applyDefaults(cfg)
	}
	return nil
}
