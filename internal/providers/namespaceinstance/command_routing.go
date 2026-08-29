package namespaceinstance

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.NamespaceInstance.CLIPath) != "" && cfg.NamespaceInstance.CLIPath != "nsc" {
		args = append(args, "--namespace-instance-cli", cfg.NamespaceInstance.CLIPath)
	}
	if strings.TrimSpace(cfg.NamespaceInstance.Endpoint) != "" {
		endpoint := core.RoutingSafeURL(cfg.NamespaceInstance.Endpoint)
		// Opaque legacy forms can retain credential bytes; preserve private
		// config when redaction would replace the original ownership scope.
		if normalizedNamespaceClaimEndpoint(endpoint) == normalizedNamespaceClaimEndpoint(cfg.NamespaceInstance.Endpoint) {
			args = append(args, "--namespace-instance-endpoint", endpoint)
		}
	}
	if strings.TrimSpace(cfg.NamespaceInstance.Region) != "" {
		args = append(args, "--namespace-instance-region", cfg.NamespaceInstance.Region)
	}
	if strings.TrimSpace(cfg.NamespaceInstance.Keychain) != "" {
		args = append(args, "--namespace-instance-keychain", cfg.NamespaceInstance.Keychain)
	}
	if strings.TrimSpace(cfg.NamespaceInstance.WorkRoot) != "" {
		args = append(args, "--namespace-instance-work-root", cfg.NamespaceInstance.WorkRoot)
	}
	return core.CommandRouting{Args: args}
}
