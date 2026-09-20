package cli

//go:generate go run ../../scripts/configgen -source config_daytona.go -output config_daytona_generated.go -type DaytonaConfig -provider daytona

// DaytonaConfig owns mechanical bindings; environment-only credentials do not
// gain file or argv bindings, and source provenance remains in core wrappers.
type DaytonaConfig struct {
	APIKey           string `env:"CRABBOX_DAYTONA_API_KEY" envAlias:"DAYTONA_API_KEY" sources:"env" reportApplied:"true"`
	JWTToken         string `env:"CRABBOX_DAYTONA_JWT_TOKEN" envAlias:"DAYTONA_JWT_TOKEN" sources:"env" reportApplied:"true"`
	OrganizationID   string `env:"CRABBOX_DAYTONA_ORGANIZATION_ID" envAlias:"DAYTONA_ORGANIZATION_ID" sources:"env"`
	APIURL           string `config:"apiUrl" env:"CRABBOX_DAYTONA_API_URL" envAlias:"DAYTONA_API_URL" flag:"daytona-api-url" sources:"user,repo,env,flag" help:"Daytona API URL" fileIgnoreEmpty:"true" fileStorage:"value" default:"https://app.daytona.io/api" reportApplied:"true"`
	Snapshot         string `config:"snapshot" env:"CRABBOX_DAYTONA_SNAPSHOT" envAlias:"DAYTONA_SNAPSHOT" flag:"daytona-snapshot" sources:"user,repo,env,flag" help:"Daytona snapshot name" fileIgnoreEmpty:"true" fileStorage:"value"`
	Target           string `config:"target" env:"CRABBOX_DAYTONA_TARGET" envAlias:"DAYTONA_TARGET" flag:"daytona-target" sources:"user,repo,env,flag" help:"Daytona compute target" fileIgnoreEmpty:"true" fileStorage:"value"`
	User             string `config:"user" env:"CRABBOX_DAYTONA_USER" flag:"daytona-user" sources:"user,repo,env,flag" help:"Daytona sandbox user" fileIgnoreEmpty:"true" fileStorage:"value" default:"daytona"`
	WorkRoot         string `config:"workRoot" env:"CRABBOX_DAYTONA_WORK_ROOT" flag:"daytona-work-root" sources:"user,repo,env,flag" help:"Daytona sandbox work root" fileIgnoreEmpty:"true" fileStorage:"value" default:"/home/daytona/crabbox"`
	SSHGatewayHost   string `config:"sshGatewayHost" env:"CRABBOX_DAYTONA_SSH_GATEWAY_HOST" flag:"daytona-ssh-gateway-host" sources:"user,repo,env,flag" help:"Daytona SSH gateway host" fileIgnoreEmpty:"true" fileStorage:"value" default:"ssh.app.daytona.io" reportApplied:"true"`
	SSHAccessMinutes int    `config:"sshAccessMinutes" env:"CRABBOX_DAYTONA_SSH_ACCESS_MINUTES" flag:"daytona-ssh-access-minutes" sources:"user,repo,env,flag" help:"Daytona SSH access token TTL in minutes" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value" default:"30"`
}

func applyDaytonaFileConfig(cfg *Config, file *fileDaytonaConfig, source configInputSource, credentialSource credentialValueSource) error {
	applied, err := cfg.Daytona.applyFile(file)
	recordConfigInput(cfg, "daytona", source, applied.InputAccepted)
	if applied.APIURL {
		cfg.credentialProvenance.daytonaAPIURL = credentialSource
	}
	if applied.SSHGatewayHost {
		cfg.credentialProvenance.daytonaSSHGateway = credentialSource
	}
	return err
}

func applyDaytonaEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.Daytona.applyEnv()
	recordConfigInput(cfg, "daytona", configInputEnvironment, applied.InputAccepted)
	if applied.APIKey {
		cfg.credentialProvenance.daytonaAPIKey = credentialSourceEnvironment
	}
	if applied.JWTToken {
		cfg.credentialProvenance.daytonaJWTToken = credentialSourceEnvironment
	}
	if applied.APIURL {
		cfg.credentialProvenance.daytonaAPIURL = credentialSourceEnvironment
	}
	if applied.SSHGatewayHost {
		cfg.credentialProvenance.daytonaSSHGateway = credentialSourceEnvironment
	}
	return err
}
