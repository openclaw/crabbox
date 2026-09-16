package exedev

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

var errReleaseLeaseOwnershipChanged = core.ErrReleaseLeaseOwnershipChanged

const (
	providerName  = "exe-dev"
	targetLinux   = core.TargetLinux
	networkPublic = core.NetworkPublic
)

var claimLeaseTargetForRepoConfigScopeIfUnchanged = core.ClaimLeaseTargetForRepoConfigScopeIfUnchanged

var waitForSSHReady = core.WaitForSSHReady
