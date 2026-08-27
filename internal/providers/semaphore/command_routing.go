package semaphore

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.Semaphore.Host) != "" {
		args = append(args, "--semaphore-host", cfg.Semaphore.Host)
	}
	if strings.TrimSpace(cfg.Semaphore.Project) != "" {
		args = append(args, "--semaphore-project", cfg.Semaphore.Project)
	}
	return core.CommandRouting{Args: args}
}
