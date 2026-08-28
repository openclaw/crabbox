package cli

//go:generate go run ../../scripts/configgen -source config_vercel_sandbox.go -output config_vercel_sandbox_generated.go -type VercelSandboxConfig -provider vercel-sandbox

// VercelSandboxConfig is the typed source for mechanical configuration wiring.
// Every field explicitly permits user/repo YAML, environment, and flags.
// Credentials and destinations are intentionally absent: runtime auth and
// credential policy stay in the provider. Cross-field validation stays there too.
type VercelSandboxConfig struct {
	Runtime         string   `config:"runtime" env:"CRABBOX_VERCEL_SANDBOX_RUNTIME" flag:"vercel-sandbox-runtime" sources:"user,repo,env,flag" help:"Vercel Sandbox runtime (node26, node24, node22, python3.13)" default:"node24"`
	Workdir         string   `config:"workdir" env:"CRABBOX_VERCEL_SANDBOX_WORKDIR" flag:"vercel-sandbox-workdir" sources:"user,repo,env,flag" help:"Absolute working directory inside the sandbox" default:"/vercel/sandbox/crabbox"`
	ProjectID       string   `config:"projectId" env:"CRABBOX_VERCEL_SANDBOX_PROJECT_ID" flag:"vercel-sandbox-project-id" sources:"user,repo,env,flag" help:"Vercel project ID used for sandbox scoping"`
	TeamID          string   `config:"teamId" env:"CRABBOX_VERCEL_SANDBOX_TEAM_ID" flag:"vercel-sandbox-team-id" sources:"user,repo,env,flag" help:"Vercel team ID used for sandbox scoping"`
	Scope           string   `config:"scope" env:"CRABBOX_VERCEL_SANDBOX_SCOPE" flag:"vercel-sandbox-scope" sources:"user,repo,env,flag" help:"Vercel account or team slug used for sandbox scoping"`
	VCPUs           float64  `config:"vcpus" env:"CRABBOX_VERCEL_SANDBOX_VCPUS" flag:"vercel-sandbox-vcpus" sources:"user,repo,env,flag" help:"requested Vercel Sandbox vCPU count (0 = service default)"`
	TimeoutSecs     int      `config:"timeoutSecs" env:"CRABBOX_VERCEL_SANDBOX_TIMEOUT_SECS" flag:"vercel-sandbox-timeout-secs" sources:"user,repo,env,flag" help:"sandbox lifetime cap in seconds (0 = service default)" nonnegative:"true"`
	ExecTimeoutSecs int      `config:"execTimeoutSecs" env:"CRABBOX_VERCEL_SANDBOX_EXEC_TIMEOUT_SECS" flag:"vercel-sandbox-exec-timeout-secs" sources:"user,repo,env,flag" help:"command timeout in seconds (0 = service default)" default:"600" nonnegative:"true"`
	Persistent      bool     `config:"persistent" env:"CRABBOX_VERCEL_SANDBOX_PERSISTENT" flag:"vercel-sandbox-persistent" sources:"user,repo,env,flag" help:"request a persistent sandbox when lifecycle support lands"`
	Snapshot        string   `config:"snapshot" env:"CRABBOX_VERCEL_SANDBOX_SNAPSHOT" flag:"vercel-sandbox-snapshot" sources:"user,repo,env,flag" help:"snapshot/checkpoint name or ID for future lifecycle use"`
	SnapshotMode    string   `config:"snapshotMode" env:"CRABBOX_VERCEL_SANDBOX_SNAPSHOT_MODE" flag:"vercel-sandbox-snapshot-mode" sources:"user,repo,env,flag" help:"snapshot/checkpoint mode for future lifecycle use"`
	NetworkPolicy   string   `config:"networkPolicy" env:"CRABBOX_VERCEL_SANDBOX_NETWORK_POLICY" flag:"vercel-sandbox-network-policy" sources:"user,repo,env,flag" help:"sandbox network policy: default, public, private, restricted, or none" default:"default"`
	NetworkAllow    []string `config:"networkAllow" env:"CRABBOX_VERCEL_SANDBOX_NETWORK_ALLOW" flag:"vercel-sandbox-network-allow" sources:"user,repo,env,flag" help:"comma-separated outbound CIDR/domain allow list"`
	NetworkDeny     []string `config:"networkDeny" env:"CRABBOX_VERCEL_SANDBOX_NETWORK_DENY" flag:"vercel-sandbox-network-deny" sources:"user,repo,env,flag" help:"comma-separated outbound IP/CIDR deny list"`
	Ports           []string `config:"ports" env:"CRABBOX_VERCEL_SANDBOX_PORTS" flag:"vercel-sandbox-ports" sources:"user,repo,env,flag" help:"comma-separated ports or ranges to expose later"`
	ForgetMissing   bool     `config:"forgetMissing" env:"CRABBOX_VERCEL_SANDBOX_FORGET_MISSING" flag:"vercel-sandbox-forget-missing" sources:"user,repo,env,flag" help:"remove the local claim when stop gets 404 (explicit stale-claim cleanup)"`
}
