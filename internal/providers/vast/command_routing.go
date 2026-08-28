package vast

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if apiURL := strings.TrimSpace(cfg.Vast.APIURL); apiURL != "" {
		args = append(args, "--vast-api-url", core.RoutingSafeURL(apiURL))
	}
	if core.DeleteOnReleaseExplicit(cfg, "vast") {
		args = append(args, "--vast-release-action", cfg.Vast.ReleaseAction)
	}
	return core.CommandRouting{Args: args}
}
