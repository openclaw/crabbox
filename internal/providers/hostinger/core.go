package hostinger

import (
	"context"
	"flag"
	"io"
	"os"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type HostingerConfig = core.HostingerConfig
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
type LeaseClaim = core.LeaseClaim

const (
	providerName  = "hostinger"
	targetLinux   = core.TargetLinux
	networkPublic = core.NetworkPublic
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
}

func newLeaseID() string {
	return core.NewLeaseID()
}

func leaseProviderName(leaseID, slug string) string {
	return core.LeaseProviderName(leaseID, slug)
}

func allocateDirectLeaseSlug(leaseID, requested string, servers []Server) (string, error) {
	return core.AllocateDirectLeaseSlug(leaseID, requested, servers)
}

var claimLeaseTargetForRepoConfig = func(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	if repoRoot == "" {
		return core.ClaimLeaseTargetForConfig(leaseID, slug, cfg, server, target, idleTimeout)
	}
	return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, target, repoRoot, idleTimeout, reclaim)
}

var updateLeaseClaimLabelsIfUnchangedAfter = core.UpdateLeaseClaimLabelsIfUnchangedAfter

func updateLeaseClaimLabelsIfUnchanged(leaseID string, expected LeaseClaim, labels map[string]string) (LeaseClaim, error) {
	return core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, expected, labels)
}

func updateLeaseClaimEndpoint(leaseID string, server Server, target SSHTarget) error {
	return core.UpdateLeaseClaimEndpoint(leaseID, server, target)
}

func resolveLeaseClaimForProvider(identifier, provider string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, provider)
}

func resolveLeaseClaimForProviderCloudID(cloudID, provider string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProviderCloudID(cloudID, provider)
}

func findServerByAlias(servers []Server, id string) (Server, string, error) {
	return core.FindServerByAlias(servers, id)
}

func directLeaseLabels(cfg Config, leaseID, slug, provider, market string, keep bool, now time.Time) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, provider, market, keep, now)
}

func touchDirectLeaseLabels(labels map[string]string, cfg Config, state string, now time.Time) map[string]string {
	return core.TouchDirectLeaseLabels(labels, cfg, state, now)
}

func sshTargetFromConfig(cfg Config, host string) SSHTarget {
	return core.SSHTargetFromConfig(cfg, host)
}

func isDefaultWorkRoot(value string) bool {
	return core.IsDefaultWorkRoot(value)
}

func isWorkRootExplicit(cfg *Config) bool {
	return core.IsWorkRootExplicit(cfg)
}

func isHostingerWorkRootExplicit(cfg *Config) bool {
	return core.IsHostingerWorkRootExplicit(cfg)
}

func markHostingerWorkRootExplicit(cfg *Config) {
	core.MarkHostingerWorkRootExplicit(cfg)
}

func ensureTestboxKeyForConfig(cfg Config, leaseID string) (string, string, error) {
	return core.EnsureTestboxKeyForConfig(cfg, leaseID)
}

func testboxKeyPath(leaseID string) (string, error) {
	return core.TestboxKeyPath(leaseID)
}

func removeStoredTestboxKey(leaseID string) {
	core.RemoveStoredTestboxKey(leaseID)
}

func useStoredTestboxKey(target *SSHTarget, leaseID string) {
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		return
	}
	if _, err := os.Stat(keyPath); err == nil {
		target.Key = keyPath
	}
}

func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}

func runSSHQuiet(ctx context.Context, target SSHTarget, remote string) error {
	return core.RunSSHQuiet(ctx, target, remote)
}

func shellQuote(value string) string {
	return core.ShellQuote(value)
}

func bootstrapWaitTimeout(cfg Config) time.Duration {
	return core.BootstrapWaitTimeout(cfg)
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}

func shouldCleanupServer(server Server, now time.Time) (bool, string) {
	return core.ShouldCleanupServer(server, now)
}
