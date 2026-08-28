package railway

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.Railway.APIURL) != "" {
		endpoint := core.RoutingSafeURL(cfg.Railway.APIURL)
		// Legacy scopes include query/fragment bytes. Keep the private config
		// endpoint when redaction would otherwise override its ownership scope.
		if railwayClaimEndpoint(endpoint) == railwayClaimEndpoint(cfg.Railway.APIURL) {
			args = append(args, "--railway-url", endpoint)
		}
	}
	if strings.TrimSpace(cfg.Railway.ProjectID) != "" {
		args = append(args, "--railway-project", cfg.Railway.ProjectID)
	}
	if strings.TrimSpace(cfg.Railway.EnvironmentID) != "" {
		args = append(args, "--railway-environment", cfg.Railway.EnvironmentID)
	}
	return core.CommandRouting{Args: args}
}
