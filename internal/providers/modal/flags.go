package modal

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterModalProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterModalConfigFlags(fs, defaults.Modal)
}

func ApplyModalProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "", ""); err != nil {
			return err
		}
	}
	_, err := core.ApplyProviderConfigFlags[core.ModalConfigFlagValues](cfg, fs, values, &cfg.Modal, providerName)
	return err
}
