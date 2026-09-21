package cli

//go:generate go run ../../scripts/configgen -source config_static.go -output config_static_generated.go -type StaticConfig -provider ssh

// StaticConfig holds configured inputs; target projection and SSH credentials remain separate.
type StaticConfig struct {
	ID       string `config:"id" env:"CRABBOX_STATIC_ID" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	Name     string `config:"name" env:"CRABBOX_STATIC_NAME" sources:"user,repo,env" fileIgnoreEmpty:"true" fileStorage:"value"`
	Host     string `config:"host" env:"CRABBOX_STATIC_HOST" flag:"static-host" help:"static SSH host" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	User     string `config:"user" env:"CRABBOX_STATIC_USER" flag:"static-user" help:"static SSH user" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	Port     string `config:"port" env:"CRABBOX_STATIC_PORT" flag:"static-port" help:"static SSH port" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
	WorkRoot string `config:"workRoot" env:"CRABBOX_STATIC_WORK_ROOT" flag:"static-work-root" help:"static target work root" sources:"user,repo,env,flag" fileIgnoreEmpty:"true" fileStorage:"value"`
}

func applyStaticFileConfig(cfg *Config, file *fileStaticConfig, source configInputSource, credentialSource credentialValueSource) error {
	applied, err := cfg.Static.applyFile(file)
	recordConfigInput(cfg, "ssh", source, applied.InputAccepted)
	if applied.Host {
		cfg.credentialProvenance.staticHost = credentialSource
	}
	return err
}

func applyStaticEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.Static.applyEnv()
	recordConfigInput(cfg, "ssh", configInputEnvironment, applied.InputAccepted)
	if applied.Host {
		cfg.credentialProvenance.staticHost = credentialSourceEnvironment
	}
	return err
}
