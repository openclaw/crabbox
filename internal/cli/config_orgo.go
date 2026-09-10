package cli

//go:generate go run ../../scripts/configgen -source config_orgo.go -output config_orgo_generated.go -type OrgoConfig -provider orgo

// OrgoConfig describes mechanical bindings for the delegated provider.
// Runtime key normalization and destination validation remain separate policy.
type OrgoConfig struct {
	APIKey      string `config:"apiKey" env:"CRABBOX_ORGO_API_KEY" envAlias:"ORGO_API_KEY" envAliasAfterConfig:"true" sources:"user,env" fileIgnoreEmpty:"true" reportApplied:"true" fileStorage:"value"`
	APIBase     string `config:"apiBase" env:"CRABBOX_ORGO_API_BASE" envAlias:"ORGO_API_BASE_URL" flag:"orgo-api-base" sources:"user,repo,env,flag" help:"Orgo API base URL" default:"https://www.orgo.ai/api" fileIgnoreEmpty:"true" reportApplied:"true" fileStorage:"value"`
	WorkspaceID string `config:"workspaceID" env:"CRABBOX_ORGO_WORKSPACE_ID" envAlias:"ORGO_WORKSPACE_ID" flag:"orgo-workspace-id" sources:"user,repo,env,flag" help:"Existing Orgo workspace ID to create computers in" fileIgnoreEmpty:"true" fileStorage:"value"`
	RAMGB       int    `config:"ramGB" env:"CRABBOX_ORGO_RAM_GB" flag:"orgo-ram" sources:"user,repo,env,flag" help:"Orgo computer RAM in GB" default:"4" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
	CPUs        int    `config:"cpus" env:"CRABBOX_ORGO_CPUS" flag:"orgo-cpu" sources:"user,repo,env,flag" help:"Orgo computer CPU count" default:"1" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
	DiskGB      int    `config:"diskGB" env:"CRABBOX_ORGO_DISK_GB" flag:"orgo-disk" sources:"user,repo,env,flag" help:"Orgo computer disk size in GB" default:"8" nonnegative:"true" fileInt:"positive" envInt:"fallback" fileStorage:"value"`
	Resolution  string `config:"resolution" env:"CRABBOX_ORGO_RESOLUTION" flag:"orgo-resolution" sources:"user,repo,env,flag" help:"Orgo desktop resolution, for example 1280x720x24" default:"1280x720x24" fileIgnoreEmpty:"true" fileStorage:"value"`
}
