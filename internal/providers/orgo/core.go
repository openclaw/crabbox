package orgo

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName  = "orgo"
	targetLinux   = core.TargetLinux
	networkPublic = core.NetworkPublic
)

func claimLeaseForRepoProviderEndpoint(leaseID, slug, providerScope, repoRoot string, idleTimeout time.Duration, reclaim bool, server core.Server) error {
	return core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, providerScope, "", repoRoot, idleTimeout, reclaim, server, core.SSHTarget{TargetOS: targetLinux})
}

func resolveLeaseClaimForProvider(identifier string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, providerName)
}

func resolveLeaseClaimForProviderCloudID(cloudID string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProviderCloudID(cloudID, providerName)
}
