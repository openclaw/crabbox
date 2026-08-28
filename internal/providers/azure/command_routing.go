package azure

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) CommandRouting(cfg core.Config, _ core.CommandRoutingRequest) core.CommandRouting {
	var env []string
	if strings.TrimSpace(cfg.AzureSubscription) != "" {
		env = append(env, "CRABBOX_AZURE_SUBSCRIPTION_ID="+cfg.AzureSubscription)
	}
	if strings.TrimSpace(cfg.AzureResourceGroup) != "" {
		env = append(env, "CRABBOX_AZURE_RESOURCE_GROUP="+cfg.AzureResourceGroup)
	}
	if strings.TrimSpace(cfg.AzureLocation) != "" {
		env = append(env, "CRABBOX_AZURE_LOCATION="+cfg.AzureLocation)
	}
	return core.CommandRouting{Env: env}
}
