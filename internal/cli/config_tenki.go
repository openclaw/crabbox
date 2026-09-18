package cli

//go:generate go run ../../scripts/configgen -source config_tenki.go -output config_tenki_generated.go -type TenkiConfig -provider tenki

// TenkiConfig owns mechanical bindings; destination provenance and provider
// normalization and validation remain in their existing policy phases.
type TenkiConfig struct {
	CLIPath   string `config:"cliPath" env:"CRABBOX_TENKI_CLI" envAlias:"TENKI_CLI" flag:"tenki-cli" sources:"user,repo,env,flag" help:"Tenki CLI path" default:"tenki" fileIgnoreEmpty:"true" fileStorage:"value"`
	Endpoint  string `config:"endpoint" env:"CRABBOX_TENKI_ENDPOINT" envAlias:"TENKI_ENDPOINT" flag:"tenki-endpoint" sources:"user,repo,env,flag" help:"Tenki sandbox API endpoint" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Gateway   string `config:"gateway" env:"CRABBOX_TENKI_GATEWAY" envAlias:"TENKI_GATEWAY" flag:"tenki-gateway" sources:"user,repo,env,flag" help:"Tenki sandbox SSH gateway WebSocket URL" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Workspace string `config:"workspace" env:"CRABBOX_TENKI_WORKSPACE" flag:"tenki-workspace" sources:"user,repo,env,flag" help:"legacy Tenki workspace claim scope for existing leases" fileIgnoreEmpty:"true" fileStorage:"value"`
	Project   string `config:"project" env:"CRABBOX_TENKI_PROJECT" flag:"tenki-project" sources:"user,repo,env,flag" help:"legacy Tenki project claim scope for existing leases" fileIgnoreEmpty:"true" fileStorage:"value"`
	Image     string `config:"image" env:"CRABBOX_TENKI_IMAGE" flag:"tenki-image" sources:"user,repo,env,flag" help:"Tenki sandbox registry image ref" fileIgnoreEmpty:"true" fileStorage:"value"`
	Snapshot  string `config:"snapshot" env:"CRABBOX_TENKI_SNAPSHOT" flag:"tenki-snapshot" sources:"user,repo,env,flag" help:"Tenki sandbox snapshot ID" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkRoot  string `config:"workRoot" env:"CRABBOX_TENKI_WORK_ROOT" flag:"tenki-work-root" sources:"user,repo,env,flag" help:"Tenki remote work root" default:"/home/tenki/crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	CPUs      int    `config:"cpus" env:"CRABBOX_TENKI_CPUS" flag:"tenki-cpus" sources:"user,repo,env,flag" help:"Tenki sandbox CPU cores" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
	MemoryMB  int    `config:"memoryMB" env:"CRABBOX_TENKI_MEMORY_MB" flag:"tenki-memory-mb" sources:"user,repo,env,flag" help:"Tenki sandbox memory in MB" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
	DiskGB    int    `config:"diskGB" env:"CRABBOX_TENKI_DISK_GB" flag:"tenki-disk-gb" sources:"user,repo,env,flag" help:"Tenki sandbox root disk size in GB" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
}

func applyTenkiFileConfig(cfg *Config, file *fileTenkiConfig, source configInputSource, credentialSource credentialValueSource) error {
	applied, err := cfg.Tenki.applyFile(file)
	recordConfigInput(cfg, "tenki", source, applied.InputAccepted)
	if applied.Endpoint {
		cfg.credentialProvenance.tenkiEndpoint = credentialSource
	}
	if applied.Gateway {
		cfg.credentialProvenance.tenkiGateway = credentialSource
	}
	return err
}

func applyTenkiEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.Tenki.applyEnv()
	recordConfigInput(cfg, "tenki", configInputEnvironment, applied.InputAccepted)
	if applied.Endpoint {
		cfg.credentialProvenance.tenkiEndpoint = credentialSourceEnvironment
	}
	if applied.Gateway {
		cfg.credentialProvenance.tenkiGateway = credentialSourceEnvironment
	}
	return err
}
