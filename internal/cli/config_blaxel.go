package cli

//go:generate go run ../../scripts/configgen -source config_blaxel.go -output config_blaxel_generated.go -type BlaxelConfig -provider blaxel

// BlaxelConfig describes mechanical bindings for the delegated provider.
// API keys remain environment-only; endpoint and workspace files must be trusted.
type BlaxelConfig struct {
	APIKey          string `env:"CRABBOX_BLAXEL_API_KEY" envAlias:"BL_API_KEY" sources:"env"`
	APIURL          string `config:"apiUrl" env:"CRABBOX_BLAXEL_API_URL" flag:"blaxel-api-url" sources:"user,env,flag" help:"Trusted Blaxel API base URL; not accepted from repository config" default:"https://api.blaxel.ai" fileIgnoreEmpty:"true"`
	Workspace       string `config:"workspace" env:"CRABBOX_BLAXEL_WORKSPACE" envAlias:"BL_WORKSPACE" flag:"blaxel-workspace" sources:"user,env,flag" help:"Blaxel workspace name or ID" fileIgnoreEmpty:"true"`
	Region          string `config:"region" env:"CRABBOX_BLAXEL_REGION" envAlias:"BL_REGION" flag:"blaxel-region" sources:"user,repo,env,flag" help:"Blaxel deployment region (empty = service default/policy)" fileIgnoreEmpty:"true"`
	Image           string `config:"image" env:"CRABBOX_BLAXEL_IMAGE" flag:"blaxel-image" sources:"user,repo,env,flag" help:"Blaxel sandbox image" default:"ubuntu:24.04"`
	MemoryMB        int    `config:"memoryMB" env:"CRABBOX_BLAXEL_MEMORY_MB" flag:"blaxel-memory-mb" sources:"user,repo,env,flag" help:"Blaxel sandbox memory in MB (0 = service default)" nonnegative:"true" envInt:"fallback"`
	TTL             string `config:"ttl" env:"CRABBOX_BLAXEL_TTL" flag:"blaxel-ttl" sources:"user,repo,env,flag" help:"Blaxel sandbox lifetime duration (empty = service default)" fileIgnoreEmpty:"true"`
	IdleTTL         string `config:"idleTTL" env:"CRABBOX_BLAXEL_IDLE_TTL" flag:"blaxel-idle-ttl" sources:"user,repo,env,flag" help:"Blaxel sandbox idle timeout duration (empty = service default)" fileIgnoreEmpty:"true"`
	Workdir         string `config:"workdir" env:"CRABBOX_BLAXEL_WORKDIR" flag:"blaxel-workdir" sources:"user,repo,env,flag" help:"absolute working directory inside the Blaxel sandbox" default:"/workspace/crabbox"`
	ExecTimeoutSecs int    `config:"execTimeoutSecs" env:"CRABBOX_BLAXEL_EXEC_TIMEOUT_SECS" flag:"blaxel-exec-timeout-secs" sources:"user,repo,env,flag" help:"Blaxel command timeout in seconds (0 = Crabbox default 600)" default:"600" nonnegative:"true"`
	ForgetMissing   bool   `config:"forgetMissing" env:"CRABBOX_BLAXEL_FORGET_MISSING" flag:"blaxel-forget-missing" sources:"user,repo,env,flag" help:"remove the local claim when stop gets 404 (explicit stale-claim cleanup)"`
}
