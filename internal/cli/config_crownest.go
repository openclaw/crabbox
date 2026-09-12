package cli

import "strings"

//go:generate go run ../../scripts/configgen -source config_crownest.go -output config_crownest_generated.go -type CrownestConfig -provider crownest

// CrownestConfig owns ordinary bindings; credentials and URL validation remain separate.
type CrownestConfig struct {
	APIURL        string `config:"apiUrl" env:"CRABBOX_CROWNEST_API_URL" envAlias:"CROWNEST_API_URL" flag:"crownest-url" help:"Trusted CrowNest API base URL" sources:"user,env,flag" fileIgnoreEmpty:"true" fileStorage:"value" default:"https://api.crownest.dev"`
	ProjectID     string `config:"projectId" env:"CRABBOX_CROWNEST_PROJECT_ID" envAlias:"CROWNEST_PROJECT_ID" flag:"crownest-project-id" help:"CrowNest project ID" sources:"user,repo,env,flag"`
	Template      string `config:"template" env:"CRABBOX_CROWNEST_TEMPLATE" envAlias:"CROWNEST_TEMPLATE" flag:"crownest-template" help:"CrowNest Workspace Run template" sources:"user,repo,env,flag" default:"python-node"`
	TimeoutSecs   int    `config:"timeoutSecs" env:"CRABBOX_CROWNEST_TIMEOUT_SECS" envAlias:"CROWNEST_TIMEOUT_SECS" flag:"crownest-timeout-secs" help:"CrowNest Workspace Run timeout in seconds" sources:"user,repo,env,flag" nonnegative:"true" envInt:"checked-alias" default:"600"`
	ForgetMissing bool   `config:"forgetMissing" env:"CRABBOX_CROWNEST_FORGET_MISSING" envAlias:"CROWNEST_FORGET_MISSING" flag:"crownest-forget-missing" help:"remove the local claim when stop gets 404" sources:"user,repo,env,flag"`
}

// Ignore trimmed-blank URLs only in this application snapshot, not in persistence.
func applyCrownestFileConfig(cfg *Config, file *fileCrownestConfig, trusted bool, source configInputSource) error {
	if file == nil {
		return nil
	}
	snapshot := *file
	if strings.TrimSpace(snapshot.APIURL) == "" {
		snapshot.APIURL = ""
	}
	applied, err := cfg.Crownest.applyFile(&snapshot, trusted)
	recordConfigInput(cfg, "crownest", source, applied.InputAccepted)
	return err
}
