package nvidiabrev

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName   = "nvidia-brev"
	targetLinux    = core.TargetLinux
	networkPublic  = core.NetworkPublic
	defaultSSHPort = "22"
)

func releaseActionExplicit(cfg core.Config) bool {
	return core.DeleteOnReleaseExplicit(cfg, providerName)
}

func markReleaseActionExplicit(cfg *core.Config) {
	core.MarkDeleteOnReleaseExplicit(cfg, providerName)
}

var newLeaseID = core.NewLeaseID

func directLeaseLabels(cfg core.Config, leaseID, slug, provider, market string, keep bool) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, provider, market, keep, time.Now().UTC())
}

func touchDirectLeaseLabels(labels map[string]string, cfg core.Config, state string) map[string]string {
	return core.TouchDirectLeaseLabels(labels, cfg, state, time.Now().UTC())
}

func claimLeaseTargetForRepoConfig(leaseID, slug string, cfg core.Config, server core.Server, target core.SSHTarget, repoRoot string, reclaim bool) error {
	return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, target, repoRoot, cfg.IdleTimeout, reclaim)
}

var persistLeaseTargetForRepoConfig = claimLeaseTargetForRepoConfig

func claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug string, cfg core.Config, server core.Server, target core.SSHTarget, repoRoot string, reclaim bool, expected core.LeaseClaim, expectedExists bool) (core.LeaseClaim, error) {
	return core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, server, target, repoRoot, cfg.IdleTimeout, reclaim, expected, expectedExists)
}

func resolveLeaseClaimForProvider(identifier string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, providerName)
}

func resolveLeaseClaimForProviderCloudID(cloudID string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProviderCloudID(cloudID, providerName)
}

var waitForSSH = core.WaitForSSH
