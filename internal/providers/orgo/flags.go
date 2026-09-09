package orgo

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// RegisterOrgoProviderFlags exposes non-secret Orgo settings. The API key is
// intentionally not a flag; secrets are read from env/config only.
func RegisterOrgoProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterOrgoConfigFlags(fs, defaults.Orgo)
}

func ApplyOrgoProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case providerName, "orgo-ai":
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s", providerName)
		}
	}
	v, ok := values.(core.OrgoConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.Orgo, fs)
	return nil
}
