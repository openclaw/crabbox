package azure

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

func (Provider) ClaimScope(cfg core.Config) string {
	return (&core.AzureClient{SubscriptionID: cfg.AzureSubscription, ResourceGroup: cfg.AzureResourceGroup}).LeaseClaimScope()
}
