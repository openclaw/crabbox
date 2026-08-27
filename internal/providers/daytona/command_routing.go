package daytona

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.Daytona.APIURL) != "" {
		args = append(args, "--daytona-api-url", core.RoutingSafeURL(cfg.Daytona.APIURL))
	}
	if strings.TrimSpace(cfg.Daytona.Target) != "" {
		args = append(args, "--daytona-target", cfg.Daytona.Target)
	}
	if strings.TrimSpace(cfg.Daytona.User) != "" {
		args = append(args, "--daytona-user", cfg.Daytona.User)
	}
	if strings.TrimSpace(cfg.Daytona.WorkRoot) != "" {
		args = append(args, "--daytona-work-root", cfg.Daytona.WorkRoot)
	}
	if strings.TrimSpace(cfg.Daytona.SSHGatewayHost) != "" {
		args = append(args, "--daytona-ssh-gateway-host", cfg.Daytona.SSHGatewayHost)
	}
	return core.CommandRouting{Args: args}
}
