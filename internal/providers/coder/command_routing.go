package coder

import (
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	args = append(args, fmt.Sprintf("--coder-delete-on-release=%t", cfg.Coder.DeleteOnRelease))
	if strings.TrimSpace(cfg.Coder.CLIPath) != "" {
		args = append(args, "--coder-cli", cfg.Coder.CLIPath)
	}
	if strings.TrimSpace(cfg.Coder.WorkRoot) != "" {
		args = append(args, "--coder-work-root", cfg.Coder.WorkRoot)
	}
	return core.CommandRouting{Args: args}
}
