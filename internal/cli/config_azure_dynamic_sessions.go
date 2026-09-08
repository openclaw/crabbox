package cli

//go:generate go run ../../scripts/configgen -source config_azure_dynamic_sessions.go -output config_azure_dynamic_sessions_generated.go -type AzureDynamicSessionsConfig -provider azure-dynamic-sessions

// AzureDynamicSessionsConfig describes mechanical bindings for the session pool.
// Legacy Pool rejection and native credential validation remain operation policy.
type AzureDynamicSessionsConfig struct {
	Endpoint    string `config:"endpoint" env:"CRABBOX_AZURE_DYNAMIC_SESSIONS_ENDPOINT" flag:"azure-dynamic-sessions-endpoint" sources:"user,repo,env,flag" help:"Azure Container Apps Dynamic Sessions pool management endpoint" fileIgnoreEmpty:"true" reportApplied:"true"`
	Pool        string `config:"pool" env:"CRABBOX_AZURE_DYNAMIC_SESSIONS_POOL" sources:"user,repo,env" fileIgnoreEmpty:"true"`
	APIVersion  string `config:"apiVersion" env:"CRABBOX_AZURE_DYNAMIC_SESSIONS_API_VERSION" flag:"azure-dynamic-sessions-api-version" sources:"user,repo,env,flag" help:"Azure Dynamic Sessions management API version" default:"2025-02-02-preview" fileIgnoreEmpty:"true"`
	Workdir     string `config:"workdir" env:"CRABBOX_AZURE_DYNAMIC_SESSIONS_WORKDIR" flag:"azure-dynamic-sessions-workdir" sources:"user,repo,env,flag" help:"Absolute working directory inside the Dynamic Sessions sandbox" default:"/workspace/crabbox" fileIgnoreEmpty:"true"`
	TimeoutSecs int    `config:"timeoutSecs" env:"CRABBOX_AZURE_DYNAMIC_SESSIONS_TIMEOUT_SECS" flag:"azure-dynamic-sessions-timeout-secs" sources:"user,repo,env,flag" help:"Command timeout in seconds" default:"1800" nonnegative:"true" fileInt:"positive" envInt:"fallback"`
}
