package modal

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterModalProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterModalConfigFlags(fs, defaults.Modal)
}

func ApplyModalProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	v, ok := values.(core.ModalConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.Modal, fs)
	return nil
}
