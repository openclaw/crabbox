package azure

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var env []string
	if strings.TrimSpace(cfg.Azure.Subscription) != "" {
		env = append(env, "CRABBOX_AZURE_SUBSCRIPTION_ID="+cfg.Azure.Subscription)
	}
	if strings.TrimSpace(cfg.Azure.ResourceGroup) != "" {
		env = append(env, "CRABBOX_AZURE_RESOURCE_GROUP="+cfg.Azure.ResourceGroup)
	}
	if strings.TrimSpace(cfg.Azure.Location) != "" {
		env = append(env, "CRABBOX_AZURE_LOCATION="+cfg.Azure.Location)
	}
	return core.CommandRouting{Env: env}
}
