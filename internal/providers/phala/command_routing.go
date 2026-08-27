package phala

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var args []string
	if strings.TrimSpace(cfg.Phala.NodeID) != "" {
		args = append(args, "--phala-node-id", cfg.Phala.NodeID)
	}
	if strings.TrimSpace(cfg.Phala.WorkRoot) != "" {
		args = append(args, "--phala-work-root", cfg.Phala.WorkRoot)
	}
	if strings.TrimSpace(cfg.Phala.CLIPath) != "" {
		args = append(args, "--phala-cli", cfg.Phala.CLIPath)
	}
	return core.CommandRouting{Args: args}
}
