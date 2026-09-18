package cli

//go:generate go run ../../scripts/configgen -source config_islo.go -output config_islo_generated.go -type IsloConfig -provider islo

// IsloConfig owns bindings; source provenance and explicit-resource policy use accepted facts.
type IsloConfig struct {
	APIKey         string `env:"CRABBOX_ISLO_API_KEY" envAlias:"ISLO_API_KEY" sources:"env" reportApplied:"true"`
	BaseURL        string `config:"baseUrl" env:"CRABBOX_ISLO_BASE_URL" envAlias:"ISLO_BASE_URL" flag:"islo-base-url" help:"Islo API base URL" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"https://api.islo.dev" reportApplied:"true"`
	Image          string `config:"image" env:"CRABBOX_ISLO_IMAGE" flag:"islo-image" help:"Islo sandbox image" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Workdir        string `config:"workdir" env:"CRABBOX_ISLO_WORKDIR" flag:"islo-workdir" help:"Islo sandbox working directory under /workspace" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"crabbox"`
	GatewayProfile string `config:"gatewayProfile" env:"CRABBOX_ISLO_GATEWAY_PROFILE" flag:"islo-gateway-profile" help:"Islo gateway profile name or id" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	SnapshotName   string `config:"snapshotName" env:"CRABBOX_ISLO_SNAPSHOT_NAME" flag:"islo-snapshot-name" help:"Islo snapshot name" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	VCPUs          int    `config:"vcpus" env:"CRABBOX_ISLO_VCPUS" flag:"islo-vcpus" help:"Islo sandbox vCPUs" sources:"user,repo,env,flag" nonnegative:"true" fileInt:"positive" fileStorage:"value" envInt:"fallback" default:"2" reportApplied:"true"`
	MemoryMB       int    `config:"memoryMB" env:"CRABBOX_ISLO_MEMORY_MB" flag:"islo-memory-mb" help:"Islo sandbox memory in MB" sources:"user,repo,env,flag" nonnegative:"true" fileInt:"positive" fileStorage:"value" envInt:"fallback" default:"4096" reportApplied:"true"`
	DiskGB         int    `config:"diskGB" env:"CRABBOX_ISLO_DISK_GB" flag:"islo-disk-gb" help:"Islo sandbox disk in GB" sources:"user,repo,env,flag" nonnegative:"true" fileInt:"positive" fileStorage:"value" envInt:"fallback" default:"20" reportApplied:"true"`
	// IdlePause opts a sandbox into a provider-enforced idle pause derived from
	// IdleTimeout. Off by default: the provider adapter sends no lifecycle
	// policy unless it is set.
	IdlePause bool `config:"idlePause" env:"CRABBOX_ISLO_IDLE_PAUSE" flag:"islo-idle-pause" help:"ask Islo to pause the sandbox after --idle-timeout of inactivity (off by default)" sources:"user,repo,env,flag"`
}

func initialIsloConfig(image string) IsloConfig {
	cfg := defaultIsloConfig()
	cfg.Image = image
	return cfg
}

// MarkIsloConfigExplicit preserves explicit create inputs, including accepted defaults.
func MarkIsloConfigExplicit(cfg *Config, applied IsloConfigApplied) {
	if applied.Image {
		MarkIsloImageExplicit(cfg)
	}
	if applied.VCPUs {
		MarkIsloVCPUsExplicit(cfg)
	}
	if applied.MemoryMB {
		MarkIsloMemoryMBExplicit(cfg)
	}
	if applied.DiskGB {
		MarkIsloDiskGBExplicit(cfg)
	}
}

func applyIsloFileConfig(cfg *Config, file *fileIsloConfig, source configInputSource, credentialSource credentialValueSource) error {
	applied, err := cfg.Islo.applyFile(file)
	recordConfigInput(cfg, "islo", source, applied.InputAccepted)
	if applied.BaseURL {
		cfg.credentialProvenance.isloBaseURL = credentialSource
	}
	MarkIsloConfigExplicit(cfg, applied)
	return err
}

func applyIsloEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.Islo.applyEnv()
	recordConfigInput(cfg, "islo", configInputEnvironment, applied.InputAccepted)
	if applied.APIKey {
		cfg.credentialProvenance.isloAPIKey = credentialSourceEnvironment
	}
	if applied.BaseURL {
		cfg.credentialProvenance.isloBaseURL = credentialSourceEnvironment
	}
	MarkIsloConfigExplicit(cfg, applied)
	return err
}
