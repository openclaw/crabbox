package namespace

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	namespaceProvider = "namespace-devbox"
	targetLinux       = core.TargetLinux
	networkPublic     = core.NetworkPublic
)

func deleteOnReleaseExplicit(cfg core.Config) bool {
	return core.DeleteOnReleaseExplicit(cfg, namespaceProvider)
}

func markDeleteOnReleaseExplicit(cfg *core.Config) {
	core.MarkDeleteOnReleaseExplicit(cfg, namespaceProvider)
}

func claimLeaseForRepoProviderIfUnchanged(leaseID, slug, provider, repoRoot string, idleTimeout time.Duration, reclaim bool, expected core.LeaseClaim, expectedExists bool) (core.LeaseClaim, error) {
	return core.ClaimLeaseForRepoProviderScopePondIfUnchanged(leaseID, slug, provider, "", "", repoRoot, idleTimeout, reclaim, expected, expectedExists)
}
