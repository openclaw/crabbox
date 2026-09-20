package cli

//go:generate go run ../../scripts/configgen -source config_boxd.go -output config_boxd_generated.go -type BoxdConfig -provider boxd

// BoxdConfig contains non-secret HTTPS console routing and lease settings.
// Interactive session tokens are read only from the environment by the provider.
type BoxdConfig struct {
	APIURL          string `config:"apiUrl" env:"CRABBOX_BOXD_API_URL" flag:"boxd-api-url" sources:"user,env,flag" help:"boxd HTTPS console origin (default https://app.boxd.sh)" default:"https://app.boxd.sh" fileIgnoreEmpty:"true" fileStorage:"value" envString:"presence"`
	Org             string `config:"org" env:"CRABBOX_BOXD_ORG" flag:"boxd-org" sources:"user,env,flag" help:"boxd organization (empty = personal account)" fileIgnoreEmpty:"true" fileStorage:"value" envString:"presence"` // Empty selects the fixed personal account context.
	WorkRoot        string `config:"workRoot" env:"CRABBOX_BOXD_WORK_ROOT" flag:"boxd-work-root" sources:"user,repo,env,flag" help:"remote Crabbox work root" default:"/home/boxd/crabbox" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	DeleteOnRelease bool   `config:"deleteOnRelease" env:"CRABBOX_BOXD_DELETE_ON_RELEASE" flag:"boxd-delete-on-release" sources:"user,repo,env,flag" help:"destroy boxd machines on release instead of stopping them (default true)" default:"true" reportApplied:"true"`
}

func applyBoxdFileConfig(cfg *Config, file *fileBoxdConfig, trusted bool, source configInputSource) error {
	applied, err := cfg.Boxd.applyFile(file, trusted)
	recordConfigInput(cfg, "boxd", source, applied.InputAccepted)
	if applied.WorkRoot {
		MarkBoxdWorkRootExplicit(cfg)
		recordConfigInputIntent(cfg, "boxd", source, true)
	}
	if applied.DeleteOnRelease {
		MarkDeleteOnReleaseExplicit(cfg, "boxd")
		recordConfigInputIntent(cfg, "boxd", source, true)
	}
	return err
}

func applyBoxdEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.Boxd.applyEnv()
	recordConfigInput(cfg, "boxd", configInputEnvironment, applied.InputAccepted)
	if applied.WorkRoot {
		MarkBoxdWorkRootExplicit(cfg)
		recordConfigInputIntent(cfg, "boxd", configInputEnvironment, true)
	}
	if applied.DeleteOnRelease {
		MarkDeleteOnReleaseExplicit(cfg, "boxd")
		recordConfigInputIntent(cfg, "boxd", configInputEnvironment, true)
	}
	return err
}
