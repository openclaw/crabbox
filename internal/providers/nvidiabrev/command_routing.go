package nvidiabrev

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if cli := strings.TrimSpace(cfg.NvidiaBrev.CLI); cli != "" {
		args = append(args, "--nvidia-brev-cli", cli)
	}
	if target := strings.TrimSpace(cfg.NvidiaBrev.Target); target != "" && target != "container" {
		args = append(args, "--nvidia-brev-target", target)
	}
	if user := strings.TrimSpace(cfg.NvidiaBrev.User); user != "" {
		args = append(args, "--nvidia-brev-user", user)
	}
	if core.DeleteOnReleaseExplicit(cfg, "nvidia-brev") {
		args = append(args, "--nvidia-brev-release-action", cfg.NvidiaBrev.ReleaseAction)
	}
	return core.CommandRouting{Args: args}
}
