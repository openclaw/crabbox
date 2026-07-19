package cloudrunsandbox

import (
	"flag"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type CloudRunSandboxConfig = core.CloudRunSandboxConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type DoctorCheck = core.DoctorCheck
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type RunSessionHandle = core.RunSessionHandle
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type CleanupRequest = core.CleanupRequest
type Server = core.Server
type Repo = core.Repo
type LeaseClaim = core.LeaseClaim
type ExitError = core.ExitError
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult

const (
	providerName          = "cloud-run-sandbox"
	providerFamily        = "cloud-run-sandbox"
	leasePrefix           = "gcrs_"
	namePrefix            = "crabbox-"
	defaultCLIPath        = "/usr/local/gcp/bin/sandbox"
	defaultWorkdir        = "/tmp/crabbox"
	defaultSandboxPath    = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	targetLinux           = core.TargetLinux
	NetworkPublic         = core.NetworkPublic
	statusViewReady       = "running"
	maxSandboxNameLen     = 63
	sandboxNameSuffix     = 16
	cleanupTimeout        = 30 * time.Second
	defaultExecTimeout    = 300 * time.Second
	leaseActivityTimeout  = 15 * time.Minute
	claimStateLabel       = "cloud_run_sandbox_state"
	claimActiveUntilLabel = "cloud_run_sandbox_active_until"
	claimExpiresAtLabel   = "cloud_run_sandbox_expires_at"
	claimOwnershipLabel   = "cloud_run_sandbox_ownership_token"
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func timingReportWithRunResult(report timingReport, result RunResult, err error) timingReport {
	return core.TimingReportWithRunResult(report, result, err)
}

func handleDelegatedRunFailure(w io.Writer, req RunRequest, provider, leaseID, slug string, idleTimeout, ttl time.Duration, acquired bool, shouldStop *bool) {
	core.HandleDelegatedRunFailure(w, req, provider, leaseID, slug, idleTimeout, ttl, acquired, shouldStop)
}

func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return core.AllocateClaimLeaseSlug(leaseID, requested)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
}

func claimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProviderScopePondWithLabels(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, labels map[string]string) (LeaseClaim, error) {
	return core.ClaimLeaseForRepoProviderScopePondWithLabels(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, labels)
}

func claimLeaseForRepoProviderScopePondIfUnchanged(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, expected LeaseClaim) (LeaseClaim, error) {
	return core.ClaimLeaseForRepoProviderScopePondIfUnchanged(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim, expected, true)
}

func resolveLeaseClaim(identifier string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaim(identifier)
}

func readLeaseClaim(leaseID string) (core.LeaseClaim, error) {
	return core.ReadLeaseClaim(leaseID)
}

func readLeaseClaimWithPresence(leaseID string) (core.LeaseClaim, bool, error) {
	return core.ReadLeaseClaimWithPresence(leaseID)
}

func withLeaseClaimUnchanged(leaseID string, expected LeaseClaim, action func() error) error {
	return core.WithLeaseClaimUnchanged(leaseID, expected, action)
}

func resolveLeaseClaimAfterActionIfUnchanged(
	leaseID string,
	expected LeaseClaim,
	action func() error,
	resolve func(error) (map[string]string, bool),
) (LeaseClaim, bool, bool, error) {
	return core.ResolveLeaseClaimAfterActionIfUnchanged(leaseID, expected, action, resolve)
}

func updateLeaseClaimLabelsIfUnchanged(leaseID string, expected LeaseClaim, labels map[string]string) (LeaseClaim, error) {
	return core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, expected, labels)
}

func updateLeaseClaimLabelsAndLastUsedIfUnchanged(leaseID string, expected LeaseClaim, labels map[string]string, lastUsed time.Time) (LeaseClaim, error) {
	return core.UpdateLeaseClaimLabelsAndLastUsedIfUnchanged(leaseID, expected, labels, lastUsed)
}

func listCloudRunSandboxLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

func verifyLeaseClaimUnchanged(leaseID string, expected LeaseClaim) error {
	return core.VerifyLeaseClaimUnchanged(leaseID, expected)
}

func removeLeaseClaimIfUnchangedAfter(leaseID string, expected LeaseClaim, action func() error) error {
	return core.RemoveLeaseClaimIfUnchangedAfter(leaseID, expected, action)
}

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}

func shouldUseShell(command []string) bool {
	return core.ShouldUseShell(command)
}

func shellScriptFromArgv(command []string) string {
	return core.ShellScriptFromArgv(command)
}

func shellQuote(s string) string {
	return core.ShellQuote(s)
}

func delegatedSyncOptionsError(spec ProviderSpec, req RunRequest) error {
	return core.RejectDelegatedSyncOptionsForSpec(spec, req)
}
