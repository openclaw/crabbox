package cloudrunsandbox

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName          = "cloud-run-sandbox"
	providerFamily        = "cloud-run-sandbox"
	leasePrefix           = "gcrs_"
	namePrefix            = "crabbox-"
	defaultSandboxPath    = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	targetLinux           = core.TargetLinux
	NetworkPublic         = core.NetworkPublic
	statusViewReady       = "running"
	maxSandboxNameLen     = 63
	sandboxNameSuffix     = 16
	cleanupTimeout        = 30 * time.Second
	defaultExecTimeout    = 300 * time.Second
	leaseActivityTimeout  = 15 * time.Minute
	claimStateLabel       = "cloud_run_sandbox_state"
	claimActiveUntilLabel = "cloud_run_sandbox_active_until"
	claimExpiresAtLabel   = "cloud_run_sandbox_expires_at"
	claimOwnershipLabel   = "cloud_run_sandbox_ownership_token"
)

func claimLeaseForRepoProviderScopePondIfUnchanged(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, expected core.LeaseClaim) (core.LeaseClaim, error) {
	return core.ClaimLeaseForRepoProviderScopePondIfUnchanged(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim, expected, true)
}

func listCloudRunSandboxLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}
