package unikraftcloud

import (
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName = "unikraft-cloud"
	leasePrefix  = "ukc_"
	targetLinux  = core.TargetLinux

	networkPublic = core.NetworkPublic
)

func newLeaseID() string {
	return leasePrefix + strings.TrimPrefix(core.NewLeaseID(), "cbx_")
}

func directLeaseLabels(cfg core.Config, leaseID, slug string, keep bool, now time.Time) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
}

var claimLeaseTargetForRepoConfigScopeIfUnchangedDurable = func(leaseID, slug string, cfg core.Config, providerScope string, server core.Server, repoRoot string, idleTimeout time.Duration, reclaim bool, expected core.LeaseClaim, expectedExists bool) (core.LeaseClaim, error) {
	return core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(leaseID, slug, cfg, providerScope, server, core.SSHTarget{}, repoRoot, idleTimeout, reclaim, expected, expectedExists)
}

func listUnikraftCloudLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

var replaceLeaseClaimIfUnchangedDurable = core.ReplaceLeaseClaimIfUnchangedDurableReturning
