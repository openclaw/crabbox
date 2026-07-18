package githubcodespaces

import (
	"context"
	"flag"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type GitHubCodespacesConfig = core.GitHubCodespacesConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type DoctorCheck = core.DoctorCheck
type AcquireRequest = core.AcquireRequest
type ResolveRequest = core.ResolveRequest
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type ReleaseLeaseRequest = core.ReleaseLeaseRequest
type TouchRequest = core.TouchRequest
type CleanupRequest = core.CleanupRequest
type LeaseTarget = core.LeaseTarget
type Server = core.Server
type SSHTarget = core.SSHTarget
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type LeaseClaim = core.LeaseClaim
type Repo = core.Repo

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

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func markDeleteOnReleaseExplicit(cfg *Config) {
	core.MarkDeleteOnReleaseExplicit(cfg, providerName)
}

func markRetentionPeriodExplicit(cfg *Config) {
	core.MarkGitHubCodespacesRetentionExplicit(cfg)
}

func retentionPeriodExplicit(cfg Config) bool {
	return core.GitHubCodespacesRetentionExplicit(cfg)
}

func deleteOnReleaseExplicit(cfg Config) bool {
	return core.DeleteOnReleaseExplicit(cfg, providerName)
}

func workRootExplicit(cfg *Config) bool {
	return core.IsWorkRootExplicit(cfg)
}

func markWorkRootExplicit(cfg *Config) {
	core.MarkWorkRootExplicit(cfg)
}

func newLeaseID() string {
	return core.NewLeaseID()
}

func allocateDirectLeaseSlug(leaseID, requested string, servers []Server) (string, error) {
	return core.AllocateDirectLeaseSlug(leaseID, requested, servers)
}

func directLeaseLabels(cfg Config, leaseID, slug, provider, market string, keep bool, now time.Time) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, provider, market, keep, now)
}

func touchDirectLeaseLabels(labels map[string]string, cfg Config, state string, now time.Time) map[string]string {
	return core.TouchDirectLeaseLabels(labels, cfg, state, now)
}

func claimLeaseTargetForRepoConfig(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, target, repoRoot, idleTimeout, reclaim)
}

func claimLeaseTargetForRepoConfigIfUnchangedDurable(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
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

func providerClaimScope(cfg Config) string {
	return Provider{}.ClaimScope(cfg)
}

func resolveLeaseClaimForProvider(identifier, provider string) (LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, provider)
}

func readLeaseClaimWithPresence(leaseID string) (LeaseClaim, bool, error) {
	return core.ReadLeaseClaimWithPresence(leaseID)
}

func listLeaseClaims() ([]LeaseClaim, error) {
	return core.ListLeaseClaims()
}

func leaseClaimMatchesIdentifier(claim LeaseClaim, identifier string) bool {
	return core.LeaseClaimMatchesIdentifier(claim, identifier)
}

func isCanonicalLeaseID(value string) bool {
	return core.IsCanonicalLeaseID(value)
}

func setServerLeaseClaimSnapshot(server *Server, claim LeaseClaim, exists bool) {
	core.SetServerLeaseClaimSnapshot(server, claim, exists)
}

func serverLeaseClaimSnapshot(server Server) (LeaseClaim, bool, bool) {
	return core.ServerLeaseClaimSnapshot(server)
}

func updateLeaseClaimEndpoint(leaseID string, server Server, target SSHTarget) error {
	return core.UpdateLeaseClaimEndpoint(leaseID, server, target)
}

func updateLeaseClaimEndpointIfUnchanged(leaseID string, expected LeaseClaim, server Server, target SSHTarget) (LeaseClaim, error) {
	return core.UpdateLeaseClaimEndpointIfUnchanged(leaseID, expected, server, target)
}

func updateLeaseClaimEndpointIfUnchangedAfter(leaseID string, expected LeaseClaim, server Server, target SSHTarget, action func() error) (LeaseClaim, error) {
	return core.UpdateLeaseClaimEndpointIfUnchangedAfter(leaseID, expected, server, target, action)
}

func withLeaseClaimUnchanged(leaseID string, expected LeaseClaim, action func() error) error {
	return core.WithLeaseClaimUnchanged(leaseID, expected, action)
}

func updateLeaseClaimEndpointIfUnchangedAction(
	leaseID string,
	expected LeaseClaim,
	action func() (Server, SSHTarget, bool, error),
) (LeaseClaim, Server, SSHTarget, error) {
	return core.UpdateLeaseClaimEndpointIfUnchangedAction(leaseID, expected, action)
}

func removeLeaseClaimIfUnchangedAfter(leaseID string, expected LeaseClaim, action func() error) error {
	return core.RemoveLeaseClaimIfUnchangedAfter(leaseID, expected, action)
}

func shouldCleanupServer(server Server, now time.Time) (bool, string) {
	return core.ShouldCleanupServer(server, now)
}

func findServerByAlias(servers []Server, id string) (Server, string, error) {
	return core.FindServerByAlias(servers, id)
}

func leaseProviderName(leaseID, slug string) string {
	return core.LeaseProviderName(leaseID, slug)
}

func crabboxStateDir() (string, error) {
	return core.CrabboxStateDir()
}

func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}
