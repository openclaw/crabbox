package cli

//go:generate go run ../../scripts/configgen -source config_cloudflare_sandbox.go -output config_cloudflare_sandbox_generated.go -type CloudflareSandboxConfig -provider cloudflare-sandbox

// CloudflareSandboxConfig describes mechanical bindings for the delegated bridge.
// The optional token is admitted only from trusted user config or environment,
// never exposed as a CLI flag, and redacted by config presentation.
type CloudflareSandboxConfig struct {
	BridgeURL       string `config:"bridgeUrl" configAlias:"url" env:"CRABBOX_CLOUDFLARE_SANDBOX_URL" flag:"cloudflare-sandbox-url" sources:"user,env,flag" help:"Cloudflare Sandbox bridge URL"`
	Token           string `config:"token" env:"CRABBOX_CLOUDFLARE_SANDBOX_TOKEN" sources:"user,env"`
	Workdir         string `config:"workdir" env:"CRABBOX_CLOUDFLARE_SANDBOX_WORKDIR" flag:"cloudflare-sandbox-workdir" sources:"user,repo,env,flag" help:"Absolute working directory inside the sandbox" default:"/workspace/crabbox"`
	ExecTimeoutSecs int    `config:"execTimeoutSecs" env:"CRABBOX_CLOUDFLARE_SANDBOX_EXEC_TIMEOUT_SECS" flag:"cloudflare-sandbox-exec-timeout-secs" sources:"user,repo,env,flag" help:"command timeout in seconds (0 = bridge default)" default:"600" nonnegative:"true"`
	ForgetMissing   bool   `config:"forgetMissing" env:"CRABBOX_CLOUDFLARE_SANDBOX_FORGET_MISSING" flag:"cloudflare-sandbox-forget-missing" sources:"user,repo,env,flag" help:"remove the local claim when stop gets 404 (explicit stale-claim cleanup)"`
}
