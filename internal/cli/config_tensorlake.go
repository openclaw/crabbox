package cli

//go:generate go run ../../scripts/configgen -source config_tensorlake.go -output config_tensorlake_generated.go -type TensorlakeConfig -provider tensorlake

// TensorlakeConfig describes mechanical bindings for the delegated provider.
// Native invocation, scope validation and credential policy remain separate.
type TensorlakeConfig struct {
	APIKey         string  `env:"CRABBOX_TENSORLAKE_API_KEY" envAlias:"TENSORLAKE_API_KEY" sources:"env" reportApplied:"true"`
	APIURL         string  `config:"apiUrl" env:"CRABBOX_TENSORLAKE_API_URL" envAlias:"TENSORLAKE_API_URL" flag:"tensorlake-api-url" sources:"user,repo,env,flag" help:"Tensorlake API base URL" default:"https://api.tensorlake.ai" fileIgnoreEmpty:"true" reportApplied:"true"`
	CLIPath        string  `config:"cliPath" env:"CRABBOX_TENSORLAKE_CLI" flag:"tensorlake-cli" sources:"user,repo,env,flag" help:"Path to the tensorlake CLI binary" default:"tensorlake" fileIgnoreEmpty:"true"`
	Image          string  `config:"image" env:"CRABBOX_TENSORLAKE_IMAGE" flag:"tensorlake-image" sources:"user,repo,env,flag" help:"Tensorlake sandbox image name" fileIgnoreEmpty:"true"`
	Snapshot       string  `config:"snapshot" env:"CRABBOX_TENSORLAKE_SNAPSHOT" flag:"tensorlake-snapshot" sources:"user,repo,env,flag" help:"Tensorlake snapshot ID to restore from" fileIgnoreEmpty:"true"`
	OrganizationID string  `config:"organizationId" env:"CRABBOX_TENSORLAKE_ORGANIZATION_ID" envAlias:"TENSORLAKE_ORGANIZATION_ID" flag:"tensorlake-organization-id" sources:"user,repo,env,flag" help:"Tensorlake organization ID" fileIgnoreEmpty:"true"`
	ProjectID      string  `config:"projectId" env:"CRABBOX_TENSORLAKE_PROJECT_ID" envAlias:"TENSORLAKE_PROJECT_ID" flag:"tensorlake-project-id" sources:"user,repo,env,flag" help:"Tensorlake project ID" fileIgnoreEmpty:"true"`
	Namespace      string  `config:"namespace" env:"CRABBOX_TENSORLAKE_NAMESPACE" envAlias:"INDEXIFY_NAMESPACE" flag:"tensorlake-namespace" sources:"user,repo,env,flag" help:"Tensorlake namespace" fileIgnoreEmpty:"true"`
	Workdir        string  `config:"workdir" env:"CRABBOX_TENSORLAKE_WORKDIR" flag:"tensorlake-workdir" sources:"user,repo,env,flag" help:"Absolute working directory inside the sandbox (also used as sync target)" default:"/workspace/crabbox" fileIgnoreEmpty:"true"`
	CPUs           float64 `config:"cpus" env:"CRABBOX_TENSORLAKE_CPUS" flag:"tensorlake-cpus" sources:"user,repo,env,flag" help:"Tensorlake sandbox CPU count" default:"1.0" fileFloat:"positive"`
	MemoryMB       int     `config:"memoryMB" env:"CRABBOX_TENSORLAKE_MEMORY_MB" flag:"tensorlake-memory-mb" sources:"user,repo,env,flag" help:"Tensorlake sandbox memory in MB" default:"1024" nonnegative:"true" fileInt:"positive" envInt:"fallback"`
	DiskMB         int     `config:"diskMB" env:"CRABBOX_TENSORLAKE_DISK_MB" flag:"tensorlake-disk-mb" sources:"user,repo,env,flag" help:"Tensorlake sandbox root disk in MB" default:"10240" nonnegative:"true" fileInt:"positive" envInt:"fallback"`
	TimeoutSecs    int     `config:"timeoutSecs" env:"CRABBOX_TENSORLAKE_TIMEOUT_SECS" flag:"tensorlake-timeout-secs" sources:"user,repo,env,flag" help:"Tensorlake sandbox lifetime timeout in seconds" nonnegative:"true" fileInt:"positive" envInt:"fallback"`
	NoInternet     bool    `config:"noInternet" env:"CRABBOX_TENSORLAKE_NO_INTERNET" flag:"tensorlake-no-internet" sources:"user,repo,env,flag" help:"Block outbound internet from the sandbox"`
}
