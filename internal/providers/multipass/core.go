package multipass

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "multipass"
	targetLinux  = core.TargetLinux
	sshPort      = "22"
)

var claimLeaseTargetForRepoConfigScopeReplacingEndpointIfUnchanged = core.ClaimLeaseTargetForRepoConfigScopeReplacingEndpointIfUnchanged

var removeLeaseClaim = core.RemoveLeaseClaim

var waitForSSHReady = core.WaitForSSHReady
