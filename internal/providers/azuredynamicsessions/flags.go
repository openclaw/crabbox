package azuredynamicsessions

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

func RegisterAzureDynamicSessionsProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return core.RegisterAzureDynamicSessionsConfigFlags(fs, defaults.AzureDynamicSessions)
}

func ApplyAzureDynamicSessionsProviderFlags(cfg *Config, fs *flag.FlagSet, values any) error {
	if cfg.Provider == providerName {
		if flagWasSet(fs, "class") {
			return exit(2, "--class is not supported for provider=%s; choose pool sizing in Azure", providerName)
		}
		if flagWasSet(fs, "type") {
			return exit(2, "--type is not supported for provider=%s; choose pool sizing in Azure", providerName)
		}
	}
	v, ok := values.(core.AzureDynamicSessionsConfigFlagValues)
	if !ok {
		return nil
	}
	v.Apply(&cfg.AzureDynamicSessions, fs)
	return nil
}
