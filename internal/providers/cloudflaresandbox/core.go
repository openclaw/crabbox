package cloudflaresandbox

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName   = "cloudflare-sandbox"
	providerFamily = "cloudflare"
	leasePrefix    = "cfsbx_"
	targetLinux    = core.TargetLinux
	NetworkPublic  = "public"
)

func claimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, server core.Server) error {
	return core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim, server, core.SSHTarget{})
}

func listCloudflareSandboxLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

func cloudflareSandboxCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " --id " + core.ShellQuote(leaseID)
}
