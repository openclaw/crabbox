package blaxel

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterBlaxelProviderFlags(fs *flag.FlagSet, defaults core.Config) any {
	return core.RegisterBlaxelConfigFlags(fs, defaults.Blaxel)
}

func ApplyBlaxelProviderFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --blaxel-memory-mb", "use --blaxel-image"); err != nil {
			return err
		}
	}
	if ok, err := core.ApplyProviderConfigFlags[core.BlaxelConfigFlagValues](cfg, fs, values, &cfg.Blaxel, providerName); !ok || err != nil {
		return err
	}
	return validateBlaxelConfig(*cfg)
}
