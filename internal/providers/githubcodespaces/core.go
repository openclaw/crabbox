package githubcodespaces

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	providerName               = "github-codespaces"
	providerFamily             = "github-codespaces"
	defaultGHPath              = "gh"
	defaultWorkRoot            = "/workspaces/crabbox"
	defaultSSHConfigFileMode   = 0o600
	defaultAPIURL              = "https://api.github.com"
	defaultCodespaceMachine    = "basicLinux32gb"
	defaultIdleTimeoutMinutes  = 30
	defaultRetentionPeriodDays = 7
	targetLinux                = core.TargetLinux
	networkPublic              = core.NetworkPublic
	defaultSSHPort             = "22"
)

func markDeleteOnReleaseExplicit(cfg *core.Config) {
	core.MarkDeleteOnReleaseExplicit(cfg, providerName)
}

func deleteOnReleaseExplicit(cfg core.Config) bool {
	return core.DeleteOnReleaseExplicit(cfg, providerName)
}

func claimLeaseTargetForRepoConfigIfUnchangedDurable(leaseID, slug string, cfg core.Config, server core.Server, target core.SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected core.LeaseClaim, expectedExists bool) (core.LeaseClaim, error) {
	return core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(
		leaseID,
		slug,
		cfg,
		providerClaimScope(cfg),
		server,
		target,
		repoRoot,
		idleTimeout,
		reclaim,
		expected,
		expectedExists,
	)
}

func providerClaimScope(cfg core.Config) string {
	return Provider{}.ClaimScope(cfg)
}
