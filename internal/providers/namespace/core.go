package namespace

import (
	"context"
	"flag"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type NamespaceConfig = core.NamespaceConfig
type LeaseClaim = core.LeaseClaim
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type AcquireRequest = core.AcquireRequest
type ResolveRequest = core.ResolveRequest
type ReleaseLeaseRequest = core.ReleaseLeaseRequest
type TouchRequest = core.TouchRequest
type ListRequest = core.ListRequest
type CleanupRequest = core.CleanupRequest
type LeaseView = core.LeaseView
type LeaseTarget = core.LeaseTarget
type Server = core.Server
type SSHTarget = core.SSHTarget
type ExitError = core.ExitError
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult

const (
	namespaceProvider = "namespace-devbox"
	targetLinux       = core.TargetLinux
	networkPublic     = core.NetworkPublic
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

func deleteOnReleaseExplicit(cfg Config) bool {
	return core.DeleteOnReleaseExplicit(cfg, namespaceProvider)
}

func markDeleteOnReleaseExplicit(cfg *Config) {
	core.MarkDeleteOnReleaseExplicit(cfg, namespaceProvider)
}

func newLeaseID() string {
	return core.NewLeaseID()
}

func newLeaseSlug(leaseID string) string {
	return core.NewLeaseSlug(leaseID)
}

func normalizeLeaseSlug(value string) string {
	return core.NormalizeLeaseSlug(value)
}

func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return core.AllocateClaimLeaseSlug(leaseID, requested)
}

func leaseProviderName(leaseID, slug string) string {
	return core.LeaseProviderName(leaseID, slug)
}

func directLeaseLabels(cfg Config, leaseID, slug, provider, market string, keep bool, now time.Time) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, provider, market, keep, now)
}

func touchDirectLeaseLabels(labels map[string]string, cfg Config, state string, now time.Time) map[string]string {
	return core.TouchDirectLeaseLabels(labels, cfg, state, now)
}

func claimLeaseForRepoProvider(leaseID, slug, provider, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProvider(leaseID, slug, provider, repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProviderIfUnchanged(leaseID, slug, provider, repoRoot string, idleTimeout time.Duration, reclaim bool, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
	return core.ClaimLeaseForRepoProviderScopePondIfUnchanged(leaseID, slug, provider, "", "", repoRoot, idleTimeout, reclaim, expected, expectedExists)
}

func readLeaseClaimWithPresence(leaseID string) (LeaseClaim, bool, error) {
	return core.ReadLeaseClaimWithPresence(leaseID)
}

func restoreLeaseClaimIfUnchanged(leaseID string, current, previous LeaseClaim, previousExists bool) error {
	return core.RestoreLeaseClaimIfUnchanged(leaseID, current, previous, previousExists)
}

func resolveLeaseClaim(identifier string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaim(identifier)
}

func listLeaseClaims() ([]LeaseClaim, error) {
	return core.ListLeaseClaims()
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
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

func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}

func bootstrapWaitTimeout(cfg Config) time.Duration {
	return core.BootstrapWaitTimeout(cfg)
}

func durationMinutesCeil(duration time.Duration) int {
	return core.DurationMinutesCeil(duration)
}

func cliDoctorResult(provider string, leases int, runtime string) DoctorResult {
	return core.CLIDoctorResult(provider, leases, runtime)
}
