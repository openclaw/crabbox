package cli

import "time"

//go:generate go run ../../scripts/configgen -source config_nomad.go -output config_nomad_generated.go -type NomadConfig -provider nomad

// NomadConfig owns mechanical bindings; token values remain runtime-only.
type NomadConfig struct {
	Address           string        `config:"address" env:"CRABBOX_NOMAD_ADDR" envAlias:"NOMAD_ADDR" flag:"nomad-address" sources:"user,env,flag" help:"Nomad HTTP API address (or NOMAD_ADDR)" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Region            string        `config:"region" env:"CRABBOX_NOMAD_REGION" envAlias:"NOMAD_REGION" flag:"nomad-region" sources:"user,env,flag" help:"Nomad region (or NOMAD_REGION)" fileIgnoreEmpty:"true" fileStorage:"value"`
	Namespace         string        `config:"namespace" env:"CRABBOX_NOMAD_NAMESPACE" envAlias:"NOMAD_NAMESPACE" flag:"nomad-namespace" sources:"user,env,flag" help:"Nomad namespace (or NOMAD_NAMESPACE)" fileIgnoreEmpty:"true" fileStorage:"value"`
	TokenEnv          string        `config:"tokenEnv" env:"CRABBOX_NOMAD_TOKEN_ENV" flag:"nomad-token-env" sources:"user,env,flag" help:"environment variable containing the Nomad ACL token" default:"NOMAD_TOKEN" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	CACert            string        `config:"caCert" env:"CRABBOX_NOMAD_CA_CERT" envAlias:"NOMAD_CACERT" flag:"nomad-ca-cert" sources:"user,env,flag" help:"Nomad TLS CA certificate path (or NOMAD_CACERT)" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	CAPath            string        `config:"caPath" env:"CRABBOX_NOMAD_CA_PATH" envAlias:"NOMAD_CAPATH" flag:"nomad-ca-path" sources:"user,env,flag" help:"Nomad TLS CA directory path (or NOMAD_CAPATH)" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	ClientCert        string        `config:"clientCert" env:"CRABBOX_NOMAD_CLIENT_CERT" envAlias:"NOMAD_CLIENT_CERT" flag:"nomad-client-cert" sources:"user,env,flag" help:"Nomad TLS client certificate path" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	ClientKey         string        `config:"clientKey" env:"CRABBOX_NOMAD_CLIENT_KEY" envAlias:"NOMAD_CLIENT_KEY" flag:"nomad-client-key" sources:"user,env,flag" help:"Nomad TLS client key path" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	TLSServerName     string        `config:"tlsServerName" env:"CRABBOX_NOMAD_TLS_SERVER_NAME" envAlias:"NOMAD_TLS_SERVER_NAME" flag:"nomad-tls-server-name" sources:"user,env,flag" help:"Nomad TLS server name override" fileIgnoreEmpty:"true" fileStorage:"value"`
	SkipVerify        bool          `config:"skipVerify" env:"CRABBOX_NOMAD_SKIP_VERIFY" envAlias:"NOMAD_SKIP_VERIFY" flag:"nomad-skip-verify" sources:"user,env,flag" help:"skip Nomad TLS verification"`
	Task              string        `config:"task" env:"CRABBOX_NOMAD_TASK" flag:"nomad-task" sources:"user,env,flag" help:"Nomad task name for later delegated runs" default:"crabbox"`
	Driver            string        `config:"driver" env:"CRABBOX_NOMAD_DRIVER" flag:"nomad-driver" sources:"user,env,flag" help:"Nomad task driver for later delegated runs" default:"docker"`
	Image             string        `config:"image" env:"CRABBOX_NOMAD_IMAGE" flag:"nomad-image" sources:"user,env,flag" help:"Nomad task image for later delegated runs" default:"ubuntu:24.04"`
	Workdir           string        `config:"workdir" env:"CRABBOX_NOMAD_WORKDIR" flag:"nomad-workdir" sources:"user,env,flag" help:"absolute workdir inside later Nomad tasks" default:"/workspace/crabbox"`
	JobSpecTemplate   string        `config:"jobspecTemplate" env:"CRABBOX_NOMAD_JOBSPEC_TEMPLATE" flag:"nomad-jobspec-template" sources:"user,env,flag" help:"optional Nomad job spec template path" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	NodePool          string        `config:"nodePool" env:"CRABBOX_NOMAD_NODE_POOL" flag:"nomad-node-pool" sources:"user,env,flag" help:"optional Nomad node pool for later delegated runs" fileIgnoreEmpty:"true" fileStorage:"value"`
	Datacenters       []string      `config:"datacenters" env:"CRABBOX_NOMAD_DATACENTERS" flag:"nomad-datacenters" sources:"user,env,flag" help:"comma-separated Nomad datacenters for later delegated runs" fileList:"nonempty-normalized" fileStorage:"value" envList:"presence"`
	CPU               int           `config:"cpu" env:"CRABBOX_NOMAD_CPU" flag:"nomad-cpu" sources:"user,env,flag" help:"CPU MHz for later Nomad tasks" default:"1000" nonnegative:"true" envInt:"fallback"`
	MemoryMB          int           `config:"memoryMB" env:"CRABBOX_NOMAD_MEMORY_MB" flag:"nomad-memory-mb" sources:"user,env,flag" help:"memory MB for later Nomad tasks" default:"2048" nonnegative:"true" envInt:"fallback"`
	DiskMB            int           `config:"diskMB" env:"CRABBOX_NOMAD_DISK_MB" flag:"nomad-disk-mb" sources:"user,env,flag" help:"ephemeral disk MB for later Nomad tasks" default:"1024" nonnegative:"true" envInt:"fallback"`
	AllocReadyTimeout time.Duration `config:"allocReadyTimeout" env:"CRABBOX_NOMAD_ALLOC_READY_TIMEOUT" flag:"nomad-alloc-ready-timeout" sources:"user,env,flag" help:"allocation readiness timeout" default:"5m" duration:"positive-overlay" fileStorage:"value" flagDuration:"trim-positive" flagDurationError:"nomad alloc ready timeout must be a positive duration"`
	EvalTimeout       time.Duration `config:"evalTimeout" env:"CRABBOX_NOMAD_EVAL_TIMEOUT" flag:"nomad-eval-timeout" sources:"user,env,flag" help:"evaluation readiness timeout" default:"5m" duration:"positive-overlay" fileStorage:"value" flagDuration:"trim-positive" flagDurationError:"nomad eval timeout must be a positive duration"`
	ExecTimeoutSecs   int           `config:"execTimeoutSecs" env:"CRABBOX_NOMAD_EXEC_TIMEOUT_SECS" flag:"nomad-exec-timeout-secs" sources:"user,env,flag" help:"later task command timeout in seconds" default:"600" nonnegative:"true"`
}

// Slice defaults are composed once, with fresh storage for every configuration.
func initialNomadConfig() NomadConfig {
	cfg := defaultNomadConfig()
	cfg.Datacenters = []string{"dc1"}
	return cfg
}

func applyNomadFileConfig(cfg *Config, file *fileNomadConfig, trusted bool, source configInputSource, credentialSource credentialValueSource) error {
	applied, err := cfg.Nomad.applyFile(file, trusted)
	recordConfigInput(cfg, "nomad", source, applied.InputAccepted)
	if applied.Address {
		cfg.credentialProvenance.nomadAddress = credentialSource
	}
	if applied.TokenEnv {
		cfg.credentialProvenance.nomadTokenEnv = credentialSource
	}
	if applied.CACert {
		cfg.Nomad.CACert = expandUserPath(cfg.Nomad.CACert)
	}
	if applied.CAPath {
		cfg.Nomad.CAPath = expandUserPath(cfg.Nomad.CAPath)
	}
	if applied.ClientCert {
		cfg.Nomad.ClientCert = expandUserPath(cfg.Nomad.ClientCert)
	}
	if applied.ClientKey {
		cfg.Nomad.ClientKey = expandUserPath(cfg.Nomad.ClientKey)
	}
	if applied.JobSpecTemplate {
		cfg.Nomad.JobSpecTemplate = expandUserPath(cfg.Nomad.JobSpecTemplate)
	}
	return err
}

func applyNomadEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.Nomad.applyEnv()
	recordConfigInput(cfg, "nomad", configInputEnvironment, applied.InputAccepted)
	if applied.Address {
		cfg.credentialProvenance.nomadAddress = credentialSourceEnvironment
	}
	if applied.TokenEnv {
		cfg.credentialProvenance.nomadTokenEnv = credentialSourceEnvironment
	}
	cfg.Nomad.CACert = expandUserPath(cfg.Nomad.CACert)
	cfg.Nomad.CAPath = expandUserPath(cfg.Nomad.CAPath)
	cfg.Nomad.ClientCert = expandUserPath(cfg.Nomad.ClientCert)
	cfg.Nomad.ClientKey = expandUserPath(cfg.Nomad.ClientKey)
	cfg.Nomad.JobSpecTemplate = expandUserPath(cfg.Nomad.JobSpecTemplate)
	return err
}
