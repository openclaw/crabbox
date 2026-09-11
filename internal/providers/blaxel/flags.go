package blaxel

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func RegisterBlaxelProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterBlaxelConfigFlags(fs, defaults.Blaxel)
}

func ApplyBlaxelProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if err := shared.RejectExplicitMachineSizingFlags(fs, providerName, "use --blaxel-memory-mb", "use --blaxel-image"); err != nil {
			return err
		}
	}
	v, ok := values.(core.BlaxelConfigFlagValues)
	if !ok {
		return nil
	}
	applied, err := v.Apply(&cfg.Blaxel, fs)
	core.RecordProviderFlagInputs(cfg, applied.InputAccepted, providerName)
	if err != nil {
		return err
	}
	return validateBlaxelConfig(*cfg)
}
