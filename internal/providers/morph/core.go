package morph

import (
	"context"
	"flag"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type MorphConfig = core.MorphConfig
type LeaseClaim = core.LeaseClaim
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
type LeaseTarget = core.LeaseTarget
type Server = core.Server
type SSHTarget = core.SSHTarget
type ExitError = core.ExitError

const (
	providerName     = "morph"
	targetLinux      = core.TargetLinux
	networkAuto      = core.NetworkAuto
	networkTailscale = core.NetworkTailscale
	networkPublic    = core.NetworkPublic
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func asExitError(err error, target *ExitError) bool {
	return core.AsExitError(err, target)
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

func normalizeLeaseSlug(value string) string {
	return core.NormalizeLeaseSlug(value)
}

func isCanonicalLeaseID(value string) bool {
	return core.IsCanonicalLeaseID(value)
}

func leaseProviderName(leaseID, slug string) string {
	return core.LeaseProviderName(leaseID, slug)
}

func allocateDirectLeaseSlug(leaseID, requested string, servers []Server) (string, error) {
	return core.AllocateDirectLeaseSlug(leaseID, requested, servers)
}

func resolveLeaseClaimForProvider(identifier, provider string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, provider)
}

func updateLeaseClaimEndpointIfUnchangedAfter(leaseID string, expected LeaseClaim, server Server, target SSHTarget, action func() error) (LeaseClaim, error) {
	return core.UpdateLeaseClaimEndpointIfUnchangedAfter(leaseID, expected, server, target, action)
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
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

func updateLeaseClaimEndpoint(leaseID string, server Server, target SSHTarget) error {
	return core.UpdateLeaseClaimEndpoint(leaseID, server, target)
}

func updateLeaseClaimEndpointIfUnchanged(leaseID string, expected LeaseClaim, server Server, target SSHTarget) (LeaseClaim, error) {
	return core.UpdateLeaseClaimEndpointIfUnchanged(leaseID, expected, server, target)
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

func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}

func bootstrapWaitTimeout(cfg Config) time.Duration {
	return core.BootstrapWaitTimeout(cfg)
}

func isDefaultWorkRoot(value string) bool {
	return core.IsDefaultWorkRoot(value)
}

func deleteOnReleaseExplicit(cfg Config) bool {
	return core.DeleteOnReleaseExplicit(cfg, providerName)
}

func markDeleteOnReleaseExplicit(cfg *Config) {
	core.MarkDeleteOnReleaseExplicit(cfg, providerName)
}

func testboxKeyPath(leaseID string) (string, error) {
	return core.TestboxKeyPath(leaseID)
}

func removeStoredTestboxKey(leaseID string) {
	core.RemoveStoredTestboxKey(leaseID)
}

func isMorphProviderName(provider string) bool {
	return strings.EqualFold(strings.TrimSpace(provider), providerName)
}
