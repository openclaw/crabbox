package cli

//go:generate go run ../../scripts/configgen -source config_cloud_run_sandbox.go -output config_cloud_run_sandbox_generated.go -type CloudRunSandboxConfig -provider cloud-run-sandbox

// CloudRunSandboxConfig describes mechanical bindings for the delegated provider.
// GatewayURL has no YAML source. Secrets remain runtime-only environment input;
// endpoint validation, transport selection and operation policy stay in the provider.
type CloudRunSandboxConfig struct {
	GatewayURL  string `env:"CRABBOX_CLOUD_RUN_SANDBOX_GATEWAY_URL" envAlias:"CLOUD_RUN_SANDBOX_URL" flag:"cloud-run-sandbox-gateway-url" sources:"env,flag" help:"durable-routing Cloud Run sandbox gateway URL (HTTPS)"`
	CLIPath     string `config:"cliPath" env:"CRABBOX_CLOUD_RUN_SANDBOX_CLI" envAlias:"CLOUD_RUN_SANDBOX_BINARY" flag:"cloud-run-sandbox-cli" sources:"user,repo,env,flag" help:"path to the Cloud Run sandbox CLI binary (direct mode)" default:"/usr/local/gcp/bin/sandbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	Workdir     string `config:"workdir" env:"CRABBOX_CLOUD_RUN_SANDBOX_WORKDIR" flag:"cloud-run-sandbox-workdir" sources:"user,repo,env,flag" help:"absolute working directory inside the sandbox (sync target)" default:"/tmp/crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	AllowEgress bool   `config:"allowEgress" env:"CRABBOX_CLOUD_RUN_SANDBOX_ALLOW_EGRESS" flag:"cloud-run-sandbox-allow-egress" sources:"user,repo,env,flag" help:"allow outbound network access from the sandbox (default deny)"`
	Write       bool   `config:"write" env:"CRABBOX_CLOUD_RUN_SANDBOX_WRITE" flag:"cloud-run-sandbox-write" sources:"user,repo,env,flag" help:"allow writable mounted filesystems inside the sandbox" default:"true"`
	Rootfs      string `config:"rootfs" env:"CRABBOX_CLOUD_RUN_SANDBOX_ROOTFS" flag:"cloud-run-sandbox-rootfs" sources:"user,repo,env,flag" help:"root filesystem exposed to the sandbox (default /)" default:"/" fileIgnoreEmpty:"true" fileStorage:"value"`
}
