package mxc

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterMXCConfigFlags(fs, defaults.MXC)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.MXCConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.MXC, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	return err
}
