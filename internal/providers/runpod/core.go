package runpod

import (
	"context"
	"flag"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type RunpodConfig = core.RunpodConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type AcquireRequest = core.AcquireRequest
type ResolveRequest = core.ResolveRequest
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type ReleaseLeaseRequest = core.ReleaseLeaseRequest
type TouchRequest = core.TouchRequest
type LeaseTarget = core.LeaseTarget
type Server = core.Server
type SSHTarget = core.SSHTarget
type TailscaleConfig = core.TailscaleConfig
type LeaseClaim = core.LeaseClaim

const (
	providerName  = "runpod"
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

func newLeaseSlug(leaseID string) string {
	return core.NewLeaseSlug(leaseID)
}

func normalizeLeaseSlug(value string) string {
	return core.NormalizeLeaseSlug(value)
}

func leaseProviderName(leaseID, slug string) string {
	return core.LeaseProviderName(leaseID, slug)
}

func allocateDirectLeaseSlug(leaseID, requested string, servers []Server) (string, error) {
	return core.AllocateDirectLeaseSlug(leaseID, requested, servers)
}

func claimLeaseTargetForRepoConfig(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, target, repoRoot, idleTimeout, reclaim)
}

func resolveLeaseClaimForProvider(identifier, provider string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, provider)
}

func resolveLeaseClaimForProviderWithExact(identifier, provider string) (core.LeaseClaim, bool, bool, error) {
	return core.ResolveLeaseClaimForProviderWithExact(identifier, provider)
}

func resolveLeaseClaimForProviderCloudID(cloudID, provider string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProviderCloudID(cloudID, provider)
}

func listLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaims()
}

func removeLeaseClaimIfUnchangedAfter(leaseID string, expected core.LeaseClaim, action func() error) error {
	return core.RemoveLeaseClaimIfUnchangedAfter(leaseID, expected, action)
}

func removeStoredTestboxKey(leaseID string) {
	core.RemoveStoredTestboxKey(leaseID)
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

func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}

func runSSHQuiet(ctx context.Context, target SSHTarget, remote string) error {
	return core.RunSSHQuiet(ctx, target, remote)
}

func bootstrapWaitTimeout(cfg Config) time.Duration {
	return core.BootstrapWaitTimeout(cfg)
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}

// isRunpodProviderName reports whether the configured provider string refers to
// the runpod provider, accepting the canonical name and the documented aliases
// in a case- and whitespace-tolerant way.
func isRunpodProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "run-pod", "runpodio":
		return true
	default:
		return false
	}
}
