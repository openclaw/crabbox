package mxc

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterMXCConfigFlags(fs, defaults.MXC)
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	_, err := core.ApplyProviderConfigFlags[core.MXCConfigFlagValues](cfg, fs, values, &cfg.MXC, providerName)
	return err
}
