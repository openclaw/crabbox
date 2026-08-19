package machine0

import (
	"context"
	"flag"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type Machine0Config = core.Machine0Config
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type AcquireRequest = core.AcquireRequest
type ResolveRequest = core.ResolveRequest
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type ReleaseLeaseRequest = core.ReleaseLeaseRequest
type TouchRequest = core.TouchRequest
type PauseRequest = core.PauseRequest
type ResumeRequest = core.ResumeRequest
type CleanupRequest = core.CleanupRequest
type LeaseTarget = core.LeaseTarget
type Server = core.Server
type SSHTarget = core.SSHTarget
type LeaseClaim = core.LeaseClaim
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult

const (
	providerName = "machine0"
	targetLinux  = core.TargetLinux
	sshPort      = "22"
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool { return core.FlagWasSet(fs, name) }
func blank(value, fallback string) string           { return core.Blank(value, fallback) }
func newLeaseID() string                            { return core.NewLeaseID() }
func leaseProviderName(leaseID, slug string) string { return core.LeaseProviderName(leaseID, slug) }

func allocateDirectLeaseSlug(leaseID, requested string, servers []Server) (string, error) {
	return core.AllocateDirectLeaseSlug(leaseID, requested, servers)
}

func directLeaseLabels(cfg Config, leaseID, slug string, keep bool, now time.Time) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
}

func touchDirectLeaseLabels(labels map[string]string, cfg Config, state string, now time.Time) map[string]string {
	return core.TouchDirectLeaseLabels(labels, cfg, state, now)
}

func claimLease(leaseID, slug string, cfg Config, repoRoot string, reclaim bool, server Server, target SSHTarget) error {
	return core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, machineScope(server.CloudID), cfg.Pond, repoRoot, cfg.IdleTimeout, reclaim, server, target)
}

func resolveClaim(identifier string) (LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, providerName)
}

func listClaims() ([]LeaseClaim, error) { return core.ListLeaseClaims() }

func updateClaim(leaseID string, expected LeaseClaim, server Server, target SSHTarget) (LeaseClaim, error) {
	return core.UpdateLeaseClaimEndpointIfUnchanged(leaseID, expected, server, target)
}

func updateClaimAfter(leaseID string, expected LeaseClaim, server Server, target SSHTarget, action func() error) (LeaseClaim, error) {
	return core.UpdateLeaseClaimEndpointIfUnchangedAfter(leaseID, expected, server, target, action)
}

func updateClaimAction(leaseID string, expected LeaseClaim, action func() (Server, SSHTarget, bool, error)) (LeaseClaim, Server, SSHTarget, error) {
	return core.ReplaceLeaseClaimEndpointIfUnchangedAction(leaseID, expected, action)
}

func removeClaimAfter(leaseID string, expected LeaseClaim, action func() error) error {
	return core.RemoveLeaseClaimIfUnchangedAfter(leaseID, expected, action)
}

func removeClaimIfUnchanged(leaseID string, expected LeaseClaim) error {
	return core.RemoveLeaseClaimIfUnchanged(leaseID, expected)
}

func withClaimUnchanged(leaseID string, expected LeaseClaim, action func() error) error {
	return core.WithLeaseClaimUnchanged(leaseID, expected, action)
}

func waitForSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSHReady(ctx, target, stderr, phase, timeout)
}

func bootstrapWaitTimeout(cfg Config) time.Duration { return core.BootstrapWaitTimeout(cfg) }
