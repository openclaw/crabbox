package agentsandbox

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "agent-sandbox"
	leasePrefix  = "asbx_"
	namePrefix   = "crabbox-"

	agentSandboxCoreGroupVersion       = "agents.x-k8s.io/v1beta1"
	agentSandboxExtensionsGroupVersion = "extensions.agents.x-k8s.io/v1beta1"

	sandboxResource      = "sandboxes"
	sandboxClaimResource = "sandboxclaims"
	warmPoolResource     = "sandboxwarmpools"
	podResource          = "pods"
	targetLinux          = core.TargetLinux
	networkPublic        = core.NetworkPublic
	statusViewReady      = "running"

	agentSandboxCleanupTimeout = 15 * time.Second
	agentSandboxStatusPoll     = 2 * time.Second
	agentSandboxClaimUIDLabel  = "agents.x-k8s.io/claim-uid"
)

func handleDelegatedRunFailure(w io.Writer, cfg core.Config, req core.RunRequest, leaseID, slug string, acquired bool, shouldStop *bool) {
	if !req.KeepOnFailure {
		return
	}
	if acquired && !req.Keep && shouldStop != nil {
		*shouldStop = false
	}
	id := slug
	if id == "" {
		id = leaseID
	}
	fmt.Fprintf(w, "keep-on-failure: kept lease=%s slug=%s expires=idle/ttl idle_timeout=%s ttl=%s\n", leaseID, core.Blank(slug, "-"), cfg.IdleTimeout, cfg.TTL)
	fmt.Fprintf(w, "rerun: %s --id %s -- <command>\n", agentSandboxRecoveryCommand(cfg, "run"), core.ShellQuote(id))
	fmt.Fprintf(w, "stop: %s %s\n", agentSandboxRecoveryCommand(cfg, "stop"), core.ShellQuote(id))
}

func agentSandboxRecoveryCommand(cfg core.Config, command string) string {
	args := []string{
		"crabbox", command,
		"--provider", providerName,
		"--agent-sandbox-kubectl", cfg.AgentSandbox.Kubectl,
	}
	if cfg.AgentSandbox.Kubeconfig != "" {
		args = append(args, "--agent-sandbox-kubeconfig", cfg.AgentSandbox.Kubeconfig)
	}
	args = append(args,
		"--agent-sandbox-context", cfg.AgentSandbox.Context,
		"--agent-sandbox-namespace", cfg.AgentSandbox.Namespace,
		"--agent-sandbox-warm-pool", cfg.AgentSandbox.WarmPool,
	)
	if cfg.AgentSandbox.Container != "" {
		args = append(args, "--agent-sandbox-container", cfg.AgentSandbox.Container)
	}
	args = append(args, "--agent-sandbox-workdir", cfg.AgentSandbox.Workdir)
	words := make([]string, 0, len(args)+1)
	if cfg.AgentSandbox.Kubeconfig == "" {
		if kubeconfig := strings.TrimSpace(os.Getenv("KUBECONFIG")); kubeconfig != "" {
			words = append(words, "KUBECONFIG="+core.ShellQuote(kubeconfig))
		}
	}
	for _, arg := range args {
		words = append(words, core.ShellQuote(arg))
	}
	return strings.Join(words, " ")
}

func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return core.AllocateDirectLeaseSlug(leaseID, requested, nil)
}
