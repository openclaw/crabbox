package superserve

import (
	"flag"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type CleanupRequest = core.CleanupRequest
type RunSessionHandle = core.RunSessionHandle
type Server = core.Server
type Repo = core.Repo
type LeaseClaim = core.LeaseClaim
type ExitError = core.ExitError
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase

const (
	providerName   = "superserve"
	leasePrefix    = "ssbx_"
	namePrefix     = "crabbox-"
	defaultBaseURL = "https://api.superserve.ai"
	defaultWorkdir = "/workspace/crabbox"
	targetLinux    = core.TargetLinux
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func writeTimingJSON(w io.Writer, report core.TimingReport) error {
	return core.WriteTimingJSON(w, report)
}

func timingReportWithRunResult(report core.TimingReport, result RunResult, err error) core.TimingReport {
	return core.TimingReportWithRunResult(report, result, err)
}

func timingReportWithProviderError(report core.TimingReport) core.TimingReport {
	report.RunStatus = core.RunStatusFailed
	report.ErrorKind = core.RunErrorProvider
	return report
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
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

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
}

func claimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim)
}

func readLeaseClaim(leaseID string) (LeaseClaim, error) {
	return core.ReadLeaseClaim(leaseID)
}

func listSuperserveLeaseClaims() ([]LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
}

func removeLeaseClaimIfUnchanged(leaseID string, expected LeaseClaim) error {
	return core.RemoveLeaseClaimIfUnchanged(leaseID, expected)
}

func shellQuote(value string) string {
	return core.ShellQuote(value)
}

func superserveCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " --id " + shellQuote(leaseID)
}

func shellScriptFromArgv(command []string) string {
	return core.ShellScriptFromArgv(command)
}

func shouldUseShell(command []string) bool {
	return core.ShouldUseShell(command)
}

func leadingEnvAssignment(command []string) bool {
	return core.LeadingEnvAssignment(command)
}

func handleDelegatedRunFailure(w io.Writer, req RunRequest, provider, leaseID, slug string, idleTimeout, ttl time.Duration, acquired bool, shouldStop *bool) {
	core.HandleDelegatedRunFailure(w, req, provider, leaseID, slug, idleTimeout, ttl, acquired, shouldStop)
}

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}
