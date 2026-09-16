package multipass

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "multipass"
	targetLinux  = core.TargetLinux
	sshPort      = "22"
)

var claimLeaseForRepoProviderScopePond = core.ClaimLeaseForRepoProviderScopePond

var claimLeaseForRepoProviderScopePondEndpoint = core.ClaimLeaseForRepoProviderScopePondEndpoint

var removeLeaseClaim = core.RemoveLeaseClaim

var updateLeaseClaimEndpoint = core.UpdateLeaseClaimEndpoint

var updateLeaseClaimCacheVolumes = core.UpdateLeaseClaimCacheVolumes

var waitForSSHReady = core.WaitForSSHReady
