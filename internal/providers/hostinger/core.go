package hostinger

import (
	"os"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName  = "hostinger"
	targetLinux   = core.TargetLinux
	networkPublic = core.NetworkPublic
)

var claimLeaseTargetForRepoConfigIfUnchanged = func(leaseID, slug string, cfg core.Config, server core.Server, target core.SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected core.LeaseClaim, expectedExists bool) (core.LeaseClaim, error) {
	if repoRoot == "" {
		return core.ClaimLeaseTargetForConfigIfUnchanged(leaseID, slug, cfg, server, target, idleTimeout, expected, expectedExists)
	}
	return core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, server, target, repoRoot, idleTimeout, reclaim, expected, expectedExists)
}

var updateLeaseClaimEndpointIfUnchangedAfter = core.UpdateLeaseClaimEndpointIfUnchangedAfter
var replaceLeaseClaimIfUnchanged = core.ReplaceLeaseClaimIfUnchanged

func hostingerWorkRootExplicit(cfg *core.Config) bool {
	return core.IsHostingerWorkRootExplicit(cfg) || core.IsWorkRootExplicit(cfg)
}

func useStoredTestboxKey(target *core.SSHTarget, leaseID string, allowAlternate bool) error {
	keyPath, err := core.StoredTestboxKeyPath(leaseID)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		_, err = os.Stat(keyPath)
	}
	if err == nil {
		target.Key = keyPath
		return nil
	} else if allowAlternate && target.Key != "" && os.IsNotExist(err) {
		return nil
	} else if !os.IsNotExist(err) {
		return core.Exit(2, "inspect hostinger lease %s stored SSH key %s: %v", leaseID, keyPath, err)
	}
	return core.Exit(2, "hostinger lease %s stored SSH key is missing; restore %s or configure an explicit SSH key", leaseID, keyPath)
}
