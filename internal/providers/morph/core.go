package morph

import (
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName     = "morph"
	targetLinux      = core.TargetLinux
	networkAuto      = core.NetworkAuto
	networkTailscale = core.NetworkTailscale
	networkPublic    = core.NetworkPublic
)

func claimLeaseForRepoProviderIfUnchanged(leaseID, slug, provider, repoRoot string, idleTimeout time.Duration, reclaim bool, expected core.LeaseClaim, expectedExists bool) (core.LeaseClaim, error) {
	return core.ClaimLeaseForRepoProviderScopePondIfUnchanged(leaseID, slug, provider, "", "", repoRoot, idleTimeout, reclaim, expected, expectedExists)
}

func deleteOnReleaseExplicit(cfg core.Config) bool {
	return core.DeleteOnReleaseExplicit(cfg, providerName)
}

func markDeleteOnReleaseExplicit(cfg *core.Config) {
	core.MarkDeleteOnReleaseExplicit(cfg, providerName)
}

func isMorphProviderName(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), providerName)
}
