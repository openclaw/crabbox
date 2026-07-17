package cua

import (
	"flag"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type CuaConfig = core.CuaConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type DoctorCheck = core.DoctorCheck
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type CleanupRequest = core.CleanupRequest
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type Server = core.Server
type Repo = core.Repo
type LeaseClaim = core.LeaseClaim
type ExitError = core.ExitError
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase

const (
	providerName             = "cua"
	defaultImage             = "ubuntu:24.04"
	defaultKind              = "container"
	defaultRegion            = ""
	defaultWorkdir           = "/workspace/crabbox"
	defaultBridgeCommand     = "python3"
	defaultSDKPackage        = "cua"
	defaultSDKImport         = "cua"
	defaultSDKFallbackImport = "cua_sandbox"
	targetLinux              = core.TargetLinux
	cuaTrackingIssue         = "https://github.com/openclaw/crabbox/issues/381"
	maxBridgeTimeoutSeconds  = int64((1<<63 - 1) / int64(time.Second))
)

func provisioningUnsupported() error {
	return exit(2, "provider=cua provisioning is disabled: the upstream CUA create API has no idempotency key or client-assigned identity echoed by create/list/get, so a timed-out create could orphan a billed sandbox; use doctor, list, or status; tracking issue: %s", cuaTrackingIssue)
}

func mutationUnsupported() error {
	return exit(2, "provider=cua is experimental and read-only: remote mutation is disabled because upstream deletion cannot atomically bind to an immutable sandbox identity; use doctor, list, or status; tracking issue: %s", cuaTrackingIssue)
}

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
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

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}

func newLeaseSlug(leaseID string) string {
	return core.NewLeaseSlug(leaseID)
}

func claimLeaseForRepoProviderScopePondIfUnchanged(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
	return core.ClaimLeaseForRepoProviderScopePondIfUnchanged(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim, expected, expectedExists)
}

func readLeaseClaim(leaseID string) (LeaseClaim, error) {
	return core.ReadLeaseClaim(leaseID)
}

func listCUALeaseClaims() ([]LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
}

func removeLeaseClaimIfUnchanged(leaseID string, expected LeaseClaim) error {
	return core.RemoveLeaseClaimIfUnchanged(leaseID, expected)
}

func restoreLeaseClaimIfUnchanged(leaseID string, current, previous LeaseClaim, previousExists bool) error {
	return core.RestoreLeaseClaimIfUnchanged(leaseID, current, previous, previousExists)
}

func updateLeaseClaimLabelsIfUnchanged(leaseID string, expected LeaseClaim, labels map[string]string) (LeaseClaim, error) {
	return core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, expected, labels)
}

func shellQuote(value string) string {
	return core.ShellQuote(value)
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
