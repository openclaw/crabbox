package tensorlake

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterTensorlakeProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterTensorlakeConfigFlags(fs, defaults.Tensorlake)
}

func ApplyTensorlakeProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(core.TensorlakeConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Tensorlake, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	return err
}
