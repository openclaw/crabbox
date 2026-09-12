package railway

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "railway"
	targetLinux  = core.TargetLinux

	networkPublic = core.NetworkPublic
)

func claimLeaseTargetForConfigIfUnchanged(leaseID string, cfg core.Config, server core.Server, expected core.LeaseClaim, expectedExists bool) (core.LeaseClaim, error) {
	return core.ClaimLeaseTargetForConfigIfUnchanged(leaseID, "", cfg, server, core.SSHTarget{}, 0, expected, expectedExists)
}

func resolveLeaseClaimForProviderCloudID(cloudID string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProviderCloudID(cloudID, providerName)
}

func providerClaimScope(cfg core.Config) string {
	return core.ProviderClaimScope(providerName, cfg)
}
