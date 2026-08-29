package e2b

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.E2B.APIURL) != "" {
		endpoint := core.RoutingSafeURL(cfg.E2B.APIURL)
		// Schemeless legacy scopes retain query/fragment bytes; preserve the
		// private configured endpoint if redaction would change that identity.
		if shared.NormalizedSandboxClaimEndpoint(endpoint) == shared.NormalizedSandboxClaimEndpoint(cfg.E2B.APIURL) {
			args = append(args, "--e2b-api-url", endpoint)
		}
	}
	if strings.TrimSpace(cfg.E2B.Domain) != "" {
		args = append(args, "--e2b-domain", cfg.E2B.Domain)
	}
	if strings.TrimSpace(cfg.E2B.User) != "" {
		args = append(args, "--e2b-user", cfg.E2B.User)
	}
	if strings.TrimSpace(cfg.E2B.Workdir) != "" {
		args = append(args, "--e2b-workdir", cfg.E2B.Workdir)
	}
	return core.CommandRouting{Args: args}
}
