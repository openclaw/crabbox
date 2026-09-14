package cli

//go:generate go run ../../scripts/configgen -source config_cua.go -output config_cua_generated.go -type CuaConfig -provider cua

// CuaConfig describes mechanical bindings for the read-only CUA provider.
// APIURL has no YAML source. Bridge/SDK fields retain trusted-file-only admission;
// credentials and provider validation remain outside these bindings.
type CuaConfig struct {
	APIURL             string `env:"CRABBOX_CUA_API_URL" envAlias:"CUA_BASE_URL" flag:"cua-api-url" sources:"env,flag" help:"Trusted CUA API base URL; not accepted from repository config"`
	Image              string `config:"image" env:"CRABBOX_CUA_IMAGE" flag:"cua-image" sources:"user,repo,env,flag" help:"CUA Linux sandbox image" default:"ubuntu:24.04"`
	Kind               string `config:"kind" env:"CRABBOX_CUA_KIND" flag:"cua-kind" sources:"user,repo,env,flag" help:"CUA sandbox kind: container or vm" default:"container"`
	Region             string `config:"region" env:"CRABBOX_CUA_REGION" flag:"cua-region" sources:"user,repo,env,flag" help:"CUA deployment region (empty = service default/policy)"`
	Workdir            string `config:"workdir" env:"CRABBOX_CUA_WORKDIR" flag:"cua-workdir" sources:"user,repo,env,flag" help:"Absolute working directory inside the sandbox" default:"/workspace/crabbox"`
	VCPUs              int    `config:"vcpus" env:"CRABBOX_CUA_VCPUS" flag:"cua-vcpus" sources:"user,repo,env,flag" help:"CUA sandbox vCPU count (0 = service default)" nonnegative:"true"`
	MemoryMB           int    `config:"memoryMB" env:"CRABBOX_CUA_MEMORY_MB" flag:"cua-memory-mb" sources:"user,repo,env,flag" help:"CUA sandbox memory in MB (0 = service default)" nonnegative:"true"`
	DiskGB             int    `config:"diskGB" env:"CRABBOX_CUA_DISK_GB" flag:"cua-disk-gb" sources:"user,repo,env,flag" help:"CUA sandbox disk in GB (0 = service default)" nonnegative:"true"`
	StartupTimeoutSecs int    `config:"startupTimeoutSecs" env:"CRABBOX_CUA_STARTUP_TIMEOUT_SECS" flag:"cua-startup-timeout-secs" sources:"user,repo,env,flag" help:"CUA sandbox startup timeout in seconds (0 = Crabbox default)" nonnegative:"true"`
	ExecTimeoutSecs    int    `config:"execTimeoutSecs" env:"CRABBOX_CUA_EXEC_TIMEOUT_SECS" flag:"cua-exec-timeout-secs" sources:"user,repo,env,flag" help:"CUA command timeout in seconds (0 = Crabbox default 600)" default:"600" nonnegative:"true"`
	BridgeCommand      string `config:"bridgeCommand" env:"CRABBOX_CUA_BRIDGE_COMMAND" flag:"cua-bridge-command" sources:"user,env,flag" help:"trusted local Python command for the future CUA SDK bridge" default:"python3"`
	SDKPackage         string `config:"sdkPackage" env:"CRABBOX_CUA_SDK_PACKAGE" flag:"cua-sdk-package" sources:"user,env,flag" help:"trusted local Python package name for CUA SDK diagnostics" default:"cua"`
	SDKImport          string `config:"sdkImport" env:"CRABBOX_CUA_SDK_IMPORT" flag:"cua-sdk-import" sources:"user,env,flag" help:"trusted local Python import path for CUA SDK diagnostics" default:"cua"`
	SDKFallbackImport  string `config:"sdkFallbackImport" env:"CRABBOX_CUA_SDK_FALLBACK_IMPORT" flag:"cua-sdk-fallback-import" sources:"user,env,flag" help:"trusted local fallback import path for CUA SDK diagnostics" default:"cua_sandbox"`
}
