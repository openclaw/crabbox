package agentsandbox

import (
	"flag"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type flagValues struct {
	Kubectl             *string
	Kubeconfig          *string
	Context             *string
	Namespace           *string
	WarmPool            *string
	Container           *string
	Workdir             *string
	SandboxReadyTimeout *time.Duration
	PodReadyTimeout     *time.Duration
	ExecTimeoutSecs     *int
	DeleteOnRelease     *bool
	ForgetMissing       *bool
}

func registerFlags(fs *flag.FlagSet, defaults core.Config) any {
	return flagValues{
		Kubectl:             fs.String("agent-sandbox-kubectl", defaults.AgentSandbox.Kubectl, "kubectl binary or path"),
		Kubeconfig:          fs.String("agent-sandbox-kubeconfig", defaults.AgentSandbox.Kubeconfig, "Kubernetes kubeconfig path"),
		Context:             fs.String("agent-sandbox-context", defaults.AgentSandbox.Context, "Kubernetes context"),
		Namespace:           fs.String("agent-sandbox-namespace", defaults.AgentSandbox.Namespace, "Kubernetes namespace"),
		WarmPool:            fs.String("agent-sandbox-warm-pool", defaults.AgentSandbox.WarmPool, "Agent Sandbox SandboxWarmPool name"),
		Container:           fs.String("agent-sandbox-container", defaults.AgentSandbox.Container, "container name for exec/tar operations (empty = default container)"),
		Workdir:             fs.String("agent-sandbox-workdir", defaults.AgentSandbox.Workdir, "absolute working directory inside the sandbox"),
		SandboxReadyTimeout: fs.Duration("agent-sandbox-sandbox-ready-timeout", defaults.AgentSandbox.SandboxReadyTimeout, "SandboxClaim/Sandbox readiness timeout"),
		PodReadyTimeout:     fs.Duration("agent-sandbox-pod-ready-timeout", defaults.AgentSandbox.PodReadyTimeout, "sandbox pod readiness timeout"),
		ExecTimeoutSecs:     fs.Int("agent-sandbox-exec-timeout-secs", defaults.AgentSandbox.ExecTimeoutSecs, "command timeout in seconds (0 = no provider deadline)"),
		DeleteOnRelease:     fs.Bool("agent-sandbox-delete-on-release", defaults.AgentSandbox.DeleteOnRelease, "delete the SandboxClaim on release"),
		ForgetMissing:       fs.Bool("agent-sandbox-forget-missing", defaults.AgentSandbox.ForgetMissing, "remove the local claim when stop sees a missing Kubernetes claim"),
	}
}

func applyFlags(cfg *core.Config, fs *flag.FlagSet, values any) error {
	v, ok := values.(flagValues)
	if !ok {
		return nil
	}
	if flagWasSet(fs, "agent-sandbox-kubectl") {
		cfg.AgentSandbox.Kubectl = *v.Kubectl
	}
	if flagWasSet(fs, "agent-sandbox-kubeconfig") {
		cfg.AgentSandbox.Kubeconfig = expandUserPath(*v.Kubeconfig)
	}
	if flagWasSet(fs, "agent-sandbox-context") {
		cfg.AgentSandbox.Context = *v.Context
	}
	if flagWasSet(fs, "agent-sandbox-namespace") {
		cfg.AgentSandbox.Namespace = *v.Namespace
	}
	if flagWasSet(fs, "agent-sandbox-warm-pool") {
		cfg.AgentSandbox.WarmPool = *v.WarmPool
	}
	if flagWasSet(fs, "agent-sandbox-container") {
		cfg.AgentSandbox.Container = *v.Container
	}
	if flagWasSet(fs, "agent-sandbox-workdir") {
		cfg.AgentSandbox.Workdir = *v.Workdir
	}
	if flagWasSet(fs, "agent-sandbox-sandbox-ready-timeout") {
		cfg.AgentSandbox.SandboxReadyTimeout = *v.SandboxReadyTimeout
	}
	if flagWasSet(fs, "agent-sandbox-pod-ready-timeout") {
		cfg.AgentSandbox.PodReadyTimeout = *v.PodReadyTimeout
	}
	if flagWasSet(fs, "agent-sandbox-exec-timeout-secs") {
		cfg.AgentSandbox.ExecTimeoutSecs = *v.ExecTimeoutSecs
	}
	if flagWasSet(fs, "agent-sandbox-delete-on-release") {
		cfg.AgentSandbox.DeleteOnRelease = *v.DeleteOnRelease
		core.MarkDeleteOnReleaseExplicit(cfg, providerName)
	}
	if flagWasSet(fs, "agent-sandbox-forget-missing") {
		cfg.AgentSandbox.ForgetMissing = *v.ForgetMissing
	}
	return validateConfig(*cfg)
}
