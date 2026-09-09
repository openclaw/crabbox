package modal

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterModalProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterModalConfigFlags(fs, defaults.Modal)
}

func ApplyModalProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=modal")
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=modal")
		}
	}
	v, ok := values.(core.ModalConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.Modal, fs)
	return nil
}
