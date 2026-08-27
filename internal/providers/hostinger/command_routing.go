package hostinger

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.Hostinger.APIURL) != "" {
		args = append(args, "--hostinger-url", core.RoutingSafeURL(cfg.Hostinger.APIURL))
	}
	return core.CommandRouting{Args: args}
}
