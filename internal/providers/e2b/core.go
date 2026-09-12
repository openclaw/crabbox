package e2b

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	e2bProvider = "e2b"
	targetLinux = core.TargetLinux

	NetworkPublic = core.NetworkPublic
)

var claimLeaseForRepoProvider = core.ClaimLeaseForRepoProvider

var claimLeaseTargetForRepoConfig = core.ClaimLeaseTargetForRepoConfig

var claimLeaseTargetForRepoConfigIfUnchanged = core.ClaimLeaseTargetForRepoConfigIfUnchanged

var claimLeaseTargetForConfigIfUnchanged = core.ClaimLeaseTargetForConfigIfUnchanged

func resolveLeaseClaimForProviderScopeWithExact(identifier, providerScope string) (core.LeaseClaim, bool, bool, error) {
	return core.ResolveLeaseClaimForProviderScopeWithExact(identifier, e2bProvider, providerScope)
}

func resolveLeaseClaimForProviderCloudIDScope(cloudID, providerScope string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProviderCloudIDScope(cloudID, e2bProvider, providerScope)
}

func providerClaimScope(cfg core.Config) string {
	return core.ProviderClaimScope(e2bProvider, cfg)
}
