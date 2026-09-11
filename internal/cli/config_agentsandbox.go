package cli

import "time"

//go:generate go run ../../scripts/configgen -source config_agentsandbox.go -output config_agentsandbox_generated.go -type AgentSandboxConfig -provider agentSandbox

// AgentSandboxConfig owns mechanical bindings; path expansion and markers stay explicit.
type AgentSandboxConfig struct {
	Kubectl             string        `config:"kubectl" env:"CRABBOX_AGENT_SANDBOX_KUBECTL" flag:"agent-sandbox-kubectl" sources:"user,env,flag" help:"kubectl binary or path" default:"kubectl" fileIgnoreEmpty:"true" fileStorage:"value"`
	Kubeconfig          string        `config:"kubeconfig" env:"CRABBOX_AGENT_SANDBOX_KUBECONFIG" flag:"agent-sandbox-kubeconfig" sources:"user,env,flag" help:"Kubernetes kubeconfig path" fileIgnoreEmpty:"true" fileStorage:"value" reportApplied:"true"`
	Context             string        `config:"context" env:"CRABBOX_AGENT_SANDBOX_CONTEXT" flag:"agent-sandbox-context" sources:"user,env,flag" help:"Kubernetes context" fileIgnoreEmpty:"true" fileStorage:"value"`
	Namespace           string        `config:"namespace" env:"CRABBOX_AGENT_SANDBOX_NAMESPACE" flag:"agent-sandbox-namespace" sources:"user,env,flag" help:"Kubernetes namespace" default:"default" fileIgnoreEmpty:"true" fileStorage:"value"`
	WarmPool            string        `config:"warmPool" env:"CRABBOX_AGENT_SANDBOX_WARM_POOL" flag:"agent-sandbox-warm-pool" sources:"user,env,flag" help:"Agent Sandbox SandboxWarmPool name" fileIgnoreEmpty:"true" fileStorage:"value"`
	Container           string        `config:"container" env:"CRABBOX_AGENT_SANDBOX_CONTAINER" flag:"agent-sandbox-container" sources:"user,env,flag" help:"container name for exec/tar operations (empty = default container)" fileIgnoreEmpty:"true" fileStorage:"value"`
	Workdir             string        `config:"workdir" env:"CRABBOX_AGENT_SANDBOX_WORKDIR" flag:"agent-sandbox-workdir" sources:"user,env,flag" help:"absolute working directory inside the sandbox" default:"/workspace/crabbox" fileIgnoreEmpty:"true" fileStorage:"value"`
	SandboxReadyTimeout time.Duration `config:"sandboxReadyTimeout" env:"CRABBOX_AGENT_SANDBOX_SANDBOX_READY_TIMEOUT" flag:"agent-sandbox-sandbox-ready-timeout" sources:"user,repo,env,flag" help:"SandboxClaim/Sandbox readiness timeout" duration:"positive-overlay" fileStorage:"value" default:"180s"`
	PodReadyTimeout     time.Duration `config:"podReadyTimeout" env:"CRABBOX_AGENT_SANDBOX_POD_READY_TIMEOUT" flag:"agent-sandbox-pod-ready-timeout" sources:"user,repo,env,flag" help:"sandbox pod readiness timeout" duration:"positive-overlay" fileStorage:"value" default:"180s"`
	ExecTimeoutSecs     int           `config:"execTimeoutSecs" env:"CRABBOX_AGENT_SANDBOX_EXEC_TIMEOUT_SECS" flag:"agent-sandbox-exec-timeout-secs" sources:"user,repo,env,flag" help:"command timeout in seconds (0 = no provider deadline)" default:"600" nonnegative:"true"`
	DeleteOnRelease     bool          `config:"deleteOnRelease" env:"CRABBOX_AGENT_SANDBOX_DELETE_ON_RELEASE" flag:"agent-sandbox-delete-on-release" sources:"user,repo,env,flag" help:"delete the SandboxClaim on release" default:"true" reportApplied:"true"`
	ForgetMissing       bool          `config:"forgetMissing" env:"CRABBOX_AGENT_SANDBOX_FORGET_MISSING" flag:"agent-sandbox-forget-missing" sources:"user,repo,env,flag" help:"remove the local claim when stop sees a missing Kubernetes claim"`
}

// ExpandAppliedLocalPaths also consumes partial reports when a later field fails.
func (cfg *AgentSandboxConfig) ExpandAppliedLocalPaths(applied AgentSandboxConfigApplied) {
	if applied.Kubeconfig {
		cfg.Kubeconfig = expandUserPath(cfg.Kubeconfig)
	}
}

func applyAgentSandboxFileConfig(cfg *Config, file *fileAgentSandboxConfig, trusted bool) error {
	applied, err := cfg.AgentSandbox.applyFile(file, trusted)
	cfg.AgentSandbox.ExpandAppliedLocalPaths(applied)
	if applied.DeleteOnRelease {
		MarkDeleteOnReleaseExplicit(cfg, "agent-sandbox")
	}
	return err
}
