package machine0

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "machine0"
	targetLinux  = core.TargetLinux
	sshPort      = "22"
)

func directLeaseLabels(cfg core.Config, leaseID, slug string, keep bool, now time.Time) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
}

func claimLease(leaseID, slug string, cfg core.Config, repoRoot string, reclaim bool, server core.Server, target core.SSHTarget) error {
	return core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, machineScope(server.CloudID), cfg.Pond, repoRoot, cfg.IdleTimeout, reclaim, server, target)
}
