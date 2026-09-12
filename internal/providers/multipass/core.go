package multipass

import (
	"context"

	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "multipass"
	targetLinux  = core.TargetLinux
	sshPort      = "22"
)

var claimLeaseForRepoProviderScopePond = func(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim)
}

var claimLeaseForRepoProviderScopePondEndpoint = func(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, server core.Server, target core.SSHTarget) error {
	return core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim, server, target)
}

var removeLeaseClaim = func(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
}

var updateLeaseClaimEndpoint = func(leaseID string, server core.Server, target core.SSHTarget) error {
	return core.UpdateLeaseClaimEndpoint(leaseID, server, target)
}

var updateLeaseClaimCacheVolumes = func(leaseID string, specs []string) error {
	return core.UpdateLeaseClaimCacheVolumes(leaseID, specs)
}

var waitForSSHReady = func(ctx context.Context, target *core.SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}
