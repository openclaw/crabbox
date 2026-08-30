package namespace

import (
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.Namespace.Site) != "" {
		args = append(args, "--namespace-site", cfg.Namespace.Site)
	}
	if strings.TrimSpace(cfg.Namespace.WorkRoot) != "" {
		args = append(args, "--namespace-work-root", cfg.Namespace.WorkRoot)
	}
	if core.DeleteOnReleaseExplicit(cfg, "namespace-devbox") {
		args = append(args, fmt.Sprintf("--namespace-delete-on-release=%t", cfg.Namespace.DeleteOnRelease))
	}
	return core.CommandRouting{Args: args}
}
