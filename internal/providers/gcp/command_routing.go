package gcp

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var env []string
	if strings.TrimSpace(cfg.GCP.Project) != "" {
		env = append(env, "CRABBOX_GCP_PROJECT="+cfg.GCP.Project)
	}
	if strings.TrimSpace(cfg.GCP.Zone) != "" {
		env = append(env, "CRABBOX_GCP_ZONE="+cfg.GCP.Zone)
	}
	return core.CommandRouting{Env: env}
}
