package blaxel

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterBlaxelProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterBlaxelConfigFlags(fs, defaults.Blaxel)
}

func ApplyBlaxelProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if strings.EqualFold(strings.TrimSpace(cfg.Provider), providerName) {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=blaxel; use --blaxel-memory-mb")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=blaxel; use --blaxel-image")
		}
	}
	v, ok := values.(core.BlaxelConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.Blaxel, fs)
	return validateBlaxelConfig(*cfg)
}
