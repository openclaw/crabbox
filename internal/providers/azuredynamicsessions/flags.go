package azuredynamicsessions

import "flag"

type azureDynamicSessionsFlagValues struct {
	Endpoint    *string
	APIVersion  *string
	Workdir     *string
	TimeoutSecs *int
}

func RegisterAzureDynamicSessionsProviderFlags(fs *flag.FlagSet, defaults Config) any {
	return azureDynamicSessionsFlagValues{
		Endpoint:    fs.String("azure-dynamic-sessions-endpoint", defaults.AzureDynamicSessions.Endpoint, "Azure Container Apps Dynamic Sessions pool management endpoint"),
		APIVersion:  fs.String("azure-dynamic-sessions-api-version", defaults.AzureDynamicSessions.APIVersion, "Azure Dynamic Sessions management API version"),
		Workdir:     fs.String("azure-dynamic-sessions-workdir", defaults.AzureDynamicSessions.Workdir, "Absolute working directory inside the Dynamic Sessions sandbox"),
		TimeoutSecs: fs.Int("azure-dynamic-sessions-timeout-secs", defaults.AzureDynamicSessions.TimeoutSecs, "Command timeout in seconds"),
	}
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
	v, ok := values.(azureDynamicSessionsFlagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "azure-dynamic-sessions-endpoint") {
		cfg.AzureDynamicSessions.Endpoint = *v.Endpoint
	}
	if flagWasSet(fs, "azure-dynamic-sessions-api-version") {
		cfg.AzureDynamicSessions.APIVersion = *v.APIVersion
	}
	if flagWasSet(fs, "azure-dynamic-sessions-workdir") {
		cfg.AzureDynamicSessions.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "azure-dynamic-sessions-timeout-secs") {
		cfg.AzureDynamicSessions.TimeoutSecs = *v.TimeoutSecs
	}
	return nil
}
