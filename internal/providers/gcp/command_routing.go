package gcp

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var env []string
	if strings.TrimSpace(cfg.GCPProject) != "" {
		env = append(env, "CRABBOX_GCP_PROJECT="+cfg.GCPProject)
	}
	if strings.TrimSpace(cfg.GCPZone) != "" {
		env = append(env, "CRABBOX_GCP_ZONE="+cfg.GCPZone)
	}
	return core.CommandRouting{Env: env}
}
