package exedev

import (
	"context"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

var errReleaseLeaseOwnershipChanged = core.ErrReleaseLeaseOwnershipChanged

const (
	providerName  = "exe-dev"
	targetLinux   = core.TargetLinux
	networkPublic = core.NetworkPublic
)

var claimLeaseTargetForRepoConfigScopeIfUnchanged = func(leaseID, slug string, cfg core.Config, providerScope string, server core.Server, target core.SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected core.LeaseClaim, expectedExists bool) (core.LeaseClaim, error) {
	return core.ClaimLeaseTargetForRepoConfigScopeIfUnchanged(leaseID, slug, cfg, providerScope, server, target, repoRoot, idleTimeout, reclaim, expected, expectedExists)
}

var waitForSSHReady = func(ctx context.Context, target *core.SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}
