package cli

//go:generate go run ../../scripts/configgen -source config_opensandbox.go -output config_opensandbox_generated.go -type OpenSandboxConfig -provider opensandbox

// OpenSandboxConfig describes mechanical bindings for the delegated provider.
// APIURL has no YAML source; ForgetMissing remains an explicit CLI-only choice.
// Credentials and provider/request validation stay outside these bindings.
type OpenSandboxConfig struct {
	APIURL          string `env:"CRABBOX_OPENSANDBOX_API_URL" envAlias:"OPEN_SANDBOX_API_URL" flag:"opensandbox-api-url" sources:"env,flag" help:"Trusted OpenSandbox API base URL; not accepted from repository config"`
	Image           string `config:"image" env:"CRABBOX_OPENSANDBOX_IMAGE" flag:"opensandbox-image" sources:"user,repo,env,flag" help:"OpenSandbox container image URI" default:"ubuntu:24.04"`
	Workdir         string `config:"workdir" env:"CRABBOX_OPENSANDBOX_WORKDIR" flag:"opensandbox-workdir" sources:"user,repo,env,flag" help:"Absolute working directory inside the sandbox (also used as sync target)" default:"/workspace/crabbox"`
	CPU             string `config:"cpu" env:"CRABBOX_OPENSANDBOX_CPU" flag:"opensandbox-cpu" sources:"user,repo,env,flag" help:"OpenSandbox CPU resource limit string (empty = service default)" default:"1"`
	Memory          string `config:"memory" env:"CRABBOX_OPENSANDBOX_MEMORY" flag:"opensandbox-memory" sources:"user,repo,env,flag" help:"OpenSandbox memory resource limit string (empty = service default)" default:"2Gi"`
	TimeoutSecs     int    `config:"timeoutSecs" env:"CRABBOX_OPENSANDBOX_TIMEOUT_SECS" flag:"opensandbox-timeout-secs" sources:"user,repo,env,flag" help:"OpenSandbox sandbox lifetime cap and readiness budget in seconds (0 = Crabbox TTL)" nonnegative:"true"`
	ExecTimeoutSecs int    `config:"execTimeoutSecs" env:"CRABBOX_OPENSANDBOX_EXEC_TIMEOUT_SECS" flag:"opensandbox-exec-timeout-secs" sources:"user,repo,env,flag" help:"OpenSandbox command timeout in seconds (0 = Crabbox default 600)" default:"600" nonnegative:"true"`
	PlatformOS      string `config:"platformOS" env:"CRABBOX_OPENSANDBOX_PLATFORM_OS" flag:"opensandbox-platform-os" sources:"user,repo,env,flag" help:"OpenSandbox platform OS constraint (set with --opensandbox-platform-arch; both empty = service default)" default:"linux"`
	PlatformArch    string `config:"platformArch" env:"CRABBOX_OPENSANDBOX_PLATFORM_ARCH" flag:"opensandbox-platform-arch" sources:"user,repo,env,flag" help:"OpenSandbox platform architecture constraint (set with --opensandbox-platform-os; both empty = service default)" default:"amd64"`
	SecureAccess    bool   `config:"secureAccess" env:"CRABBOX_OPENSANDBOX_SECURE_ACCESS" flag:"opensandbox-secure-access" sources:"user,repo,env,flag" help:"request secured sandbox endpoints"`
	UseServerProxy  bool   `config:"useServerProxy" env:"CRABBOX_OPENSANDBOX_USE_SERVER_PROXY" flag:"opensandbox-use-server-proxy" sources:"user,repo,env,flag" help:"route execd requests through the OpenSandbox server proxy"`
	ForgetMissing   bool   `flag:"opensandbox-forget-missing" sources:"flag" help:"remove the local claim when stop gets 404 (explicit stale-claim cleanup)"`
}
