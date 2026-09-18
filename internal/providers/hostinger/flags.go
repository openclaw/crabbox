package hostinger

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterHostingerProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterHostingerConfigFlags(fs, defaults.Hostinger)
}

func ApplyHostingerProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	valuesTyped, ok := values.(core.HostingerConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := valuesTyped.Apply(&cfg.Hostinger, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if applied.User {
		cfg.SSHUser = cfg.Hostinger.User
		core.MarkHostingerUserExplicit(cfg)
	}
	if applied.WorkRoot {
		core.MarkHostingerWorkRootExplicit(cfg)
	}
	if err != nil {
		return err
	}
	if cfg.Provider == providerName {
		applyDefaults(cfg)
	}
	return nil
}
