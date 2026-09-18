package cli

//go:generate go run ../../scripts/configgen -source config_unikraft_cloud.go -output config_unikraft_cloud_generated.go -type UnikraftCloudConfig -provider unikraft-cloud

// UnikraftCloudConfig owns mechanical bindings; endpoint derivation and provider
// guards remain separate from source admission.
type UnikraftCloudConfig struct {
	APIKey   string `config:"apiKey" env:"CRABBOX_UNIKRAFT_CLOUD_API_KEY" envAlias:"UNIKRAFT_CLOUD_API_KEY" envAlias2:"UKC_API_KEY" envAlias3:"UKC_TOKEN" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	APIURL   string `config:"apiUrl" env:"CRABBOX_UNIKRAFT_CLOUD_API_URL" envAlias:"UNIKRAFT_CLOUD_API_URL" flag:"unikraft-cloud-url" sources:"user,repo,env,flag" help:"Unikraft Cloud API URL override (default derived from the metro)" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Metro    string `config:"metro" env:"CRABBOX_UNIKRAFT_CLOUD_METRO" envAlias:"UNIKRAFT_CLOUD_METRO" envAlias2:"UKC_METRO" flag:"unikraft-cloud-metro" sources:"user,repo,env,flag" help:"Unikraft Cloud metro (fra, dal, sin, was, sfo)" default:"fra" fileIgnoreEmpty:"true" fileStorage:"value"`
	Image    string `config:"image" env:"CRABBOX_UNIKRAFT_CLOUD_IMAGE" envAlias:"UNIKRAFT_CLOUD_IMAGE" flag:"unikraft-cloud-image" sources:"user,repo,env,flag" help:"OCI image reference for warmup-created instances" fileIgnoreEmpty:"true" fileStorage:"value"`
	MemoryMB int    `config:"memoryMB" flag:"unikraft-cloud-memory" sources:"user,repo,flag" help:"instance memory in MB for warmup-created instances" nonnegative:"true" fileInt:"positive" fileStorage:"value"`
}

func applyUnikraftCloudFileConfig(cfg *Config, file *fileUnikraftCloudConfig, source configInputSource, credentialSource credentialValueSource) error {
	applied, err := cfg.UnikraftCloud.applyFile(file)
	recordConfigInput(cfg, "unikraft-cloud", source, applied.InputAccepted)
	if applied.APIKey {
		cfg.credentialProvenance.unikraftCloudAPIKey = credentialSource
	}
	if applied.APIURL {
		cfg.credentialProvenance.unikraftCloudAPIURL = credentialSource
	}
	return err
}

func applyUnikraftCloudEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.UnikraftCloud.applyEnv()
	recordConfigInput(cfg, "unikraft-cloud", configInputEnvironment, applied.InputAccepted)
	if applied.APIKey {
		cfg.credentialProvenance.unikraftCloudAPIKey = credentialSourceEnvironment
	}
	if applied.APIURL {
		cfg.credentialProvenance.unikraftCloudAPIURL = credentialSourceEnvironment
	}
	return err
}
