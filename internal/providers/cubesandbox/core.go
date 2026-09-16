package cubesandbox

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "cubesandbox"
	targetLinux  = core.TargetLinux

	NetworkPublic = core.NetworkPublic
)

var claimLeaseTargetForRepoConfig = core.ClaimLeaseTargetForRepoConfig

var claimLeaseTargetForRepoConfigIfUnchanged = core.ClaimLeaseTargetForRepoConfigIfUnchanged

var claimLeaseTargetForConfigIfUnchanged = core.ClaimLeaseTargetForConfigIfUnchanged

func resolveLeaseClaimForProviderScopeWithExact(identifier, providerScope string) (core.LeaseClaim, bool, bool, error) {
	return core.ResolveLeaseClaimForProviderScopeWithExact(identifier, providerName, providerScope)
}

func resolveLeaseClaimForProviderCloudIDScope(cloudID, providerScope string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProviderCloudIDScope(cloudID, providerName, providerScope)
}

func providerClaimScope(cfg core.Config) string {
	return core.ProviderClaimScope(providerName, cfg)
}
