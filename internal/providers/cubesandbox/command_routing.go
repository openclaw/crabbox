package cubesandbox

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.CubeSandbox.APIURL) != "" {
		endpoint := core.RoutingSafeURL(cfg.CubeSandbox.APIURL)
		// Schemeless legacy scopes retain query/fragment bytes; preserve the
		// private configured endpoint if redaction would change that identity.
		if shared.NormalizedSandboxClaimEndpoint(endpoint) == shared.NormalizedSandboxClaimEndpoint(cfg.CubeSandbox.APIURL) {
			args = append(args, "--cubesandbox-api-url", endpoint)
		}
	}
	if strings.TrimSpace(cfg.CubeSandbox.Domain) != "" {
		args = append(args, "--cubesandbox-domain", cfg.CubeSandbox.Domain)
	}
	if strings.TrimSpace(cfg.CubeSandbox.User) != "" {
		args = append(args, "--cubesandbox-user", cfg.CubeSandbox.User)
	}
	if strings.TrimSpace(cfg.CubeSandbox.Workdir) != "" {
		args = append(args, "--cubesandbox-workdir", cfg.CubeSandbox.Workdir)
	}
	return core.CommandRouting{Args: args}
}
