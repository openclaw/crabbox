package localcontainer

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.LocalContainer.Runtime) != "" {
		args = append(args, "--local-container-runtime", cfg.LocalContainer.Runtime)
	}
	return core.CommandRouting{Args: args}
}
