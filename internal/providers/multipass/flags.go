package multipass

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterMultipassConfigFlags(fs, defaults.Multipass)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.MultipassConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Multipass, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if applied.Image {
		core.MarkMultipassImageExplicit(cfg)
	}
	if applied.User {
		cfg.SSHUser = cfg.Multipass.User
	}
	if applied.WorkRoot {
		cfg.WorkRoot = cfg.Multipass.WorkRoot
	}
	if err != nil {
		return err
	}
	if core.ProviderNameMatches(cfg.Provider, Provider{}) {
		applyDefaults(cfg)
	}
	return nil
}
