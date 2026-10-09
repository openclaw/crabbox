package cli

import "strings"

//go:generate go run ../../scripts/configgen -source config_neevcloud.go -output config_neevcloud_generated.go -type NeevcloudConfig -provider neevcloud

// NeevcloudConfig owns mechanical bindings. The API key stays out of
// configuration and argv; provider validation stays in the adapter.
type NeevcloudConfig struct {
	BaseURL       string `config:"baseUrl" env:"CRABBOX_NEEVCLOUD_BASE_URL" flag:"neevcloud-base-url" sources:"user,env,flag" help:"Trusted NeevCloud API base URL" default:"https://api.ai.neevcloud.com" fileIgnoreEmpty:"true" fileStorage:"value"`
	OrgID         string `config:"orgId" env:"CRABBOX_NEEVCLOUD_ORG_ID" flag:"neevcloud-org-id" sources:"user,repo,env,flag" help:"NeevCloud organization ID"`
	ProjectID     string `config:"projectId" env:"CRABBOX_NEEVCLOUD_PROJECT_ID" flag:"neevcloud-project-id" sources:"user,repo,env,flag" help:"NeevCloud project ID"`
	Template      string `config:"template" env:"CRABBOX_NEEVCLOUD_TEMPLATE" flag:"neevcloud-template" sources:"user,repo,env,flag" help:"NeevCloud sandbox template ID" default:"sb-ubuntu-26-04-dev"`
	Workdir       string `config:"workdir" env:"CRABBOX_NEEVCLOUD_WORKDIR" flag:"neevcloud-workdir" sources:"user,repo,env,flag" help:"Absolute working directory under /workspace inside the sandbox" default:"/workspace/crabbox"`
	TimeoutSecs   int    `config:"timeoutSecs" env:"CRABBOX_NEEVCLOUD_TIMEOUT_SECS" flag:"neevcloud-timeout-secs" sources:"user,repo,env,flag" help:"NeevCloud sandbox max lifetime in seconds (0 = Crabbox TTL)" nonnegative:"true"`
	AllowInternet bool   `config:"allowInternet" env:"CRABBOX_NEEVCLOUD_ALLOW_INTERNET" flag:"neevcloud-allow-internet" sources:"user,repo,env,flag" help:"allow outbound internet access from the sandbox" default:"true"`
}

// Repository config cannot set baseUrl; a trimmed-blank URL is ignored only here.
func applyNeevcloudFileConfig(cfg *Config, file *fileNeevcloudConfig, trusted bool, source configInputSource) error {
	if file == nil {
		return nil
	}
	snapshot := *file
	if strings.TrimSpace(snapshot.BaseURL) == "" {
		snapshot.BaseURL = ""
	}
	applied, err := cfg.Neevcloud.applyFile(&snapshot, trusted)
	recordConfigInput(cfg, "neevcloud", source, applied.InputAccepted)
	return err
}

// applyNeevcloudEnvironmentConfig applies CRABBOX_NEEVCLOUD_* overrides.
func applyNeevcloudEnvironmentConfig(cfg *Config) error {
	applied, err := cfg.Neevcloud.applyEnv()
	recordConfigInput(cfg, "neevcloud", configInputEnvironment, applied.InputAccepted)
	return err
}
