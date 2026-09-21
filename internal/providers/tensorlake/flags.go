package tensorlake

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterTensorlakeProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterTensorlakeConfigFlags(fs, defaults.Tensorlake)
}

func ApplyTensorlakeProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	_, err := core.ApplyProviderConfigFlags[core.TensorlakeConfigFlagValues](cfg, fs, values, &cfg.Tensorlake, providerName)
	return err
}
