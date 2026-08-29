package aws

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var env []string
	if strings.TrimSpace(cfg.AWSRegion) != "" {
		env = append(env, "CRABBOX_AWS_REGION="+cfg.AWSRegion)
	}
	return core.CommandRouting{Env: env}
}
