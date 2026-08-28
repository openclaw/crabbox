package exedev

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.ExeDev.ControlHost) != "" {
		args = append(args, "--exe-dev-control-host", cfg.ExeDev.ControlHost)
	}
	return core.CommandRouting{Args: args}
}
