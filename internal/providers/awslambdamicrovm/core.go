package awslambdamicrovm

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
type RunSessionHandle = core.RunSessionHandle
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type PauseRequest = core.PauseRequest
type ResumeRequest = core.ResumeRequest
type CleanupRequest = core.CleanupRequest
type Server = core.Server
type Repo = core.Repo
type LeaseClaim = core.LeaseClaim
type ExitError = core.ExitError
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase

const (
	providerName = "aws-lambda-microvm"
	targetLinux  = core.TargetLinux
	runnerPort   = 8080
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool { return core.FlagWasSet(fs, name) }
func newLeaseID() string                            { return core.NewLeaseID() }
func now(rt Runtime) time.Time {
	if rt.Clock != nil {
		return rt.Clock.Now()
	}
	return time.Now()
}
func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return core.AllocateClaimLeaseSlug(leaseID, requested)
}
func resolveLeaseClaim(identifier string) (LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, providerName)
}
func listLeaseClaims() ([]LeaseClaim, error) { return core.ListLeaseClaims() }
func removeLeaseClaim(leaseID string)        { core.RemoveLeaseClaim(leaseID) }
func claimLease(leaseID, slug, scope, pond, repoRoot string, idle time.Duration, reclaim bool, server Server) error {
	return core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, scope, pond, repoRoot, idle, reclaim, server, core.SSHTarget{})
}
func directLeaseLabels(cfg Config, leaseID, slug string, keep bool, at time.Time) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "serverless", keep, at)
}
func touchLeaseLabels(labels map[string]string, cfg Config, state string, at time.Time) map[string]string {
	return core.TouchDirectLeaseLabels(labels, cfg, state, at)
}
func shouldCleanupServer(server Server, at time.Time) (bool, string) {
	return core.ShouldCleanupServer(server, at)
}
func delegatedSyncOptionsError(spec ProviderSpec, req RunRequest) error {
	return core.RejectDelegatedSyncOptionsForSpec(spec, req)
}
func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}
func timingReportWithRunResult(report timingReport, result RunResult, err error) timingReport {
	return core.TimingReportWithRunResult(report, result, err)
}
func handleDelegatedRunFailure(w io.Writer, req RunRequest, leaseID, slug string, idleTimeout, ttl time.Duration, acquired bool, shouldStop *bool) {
	core.HandleDelegatedRunFailure(w, req, providerName, leaseID, slug, idleTimeout, ttl, acquired, shouldStop)
}
func printEnvForwardingSummary(w io.Writer, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, providerName, "forwarded", allow, env)
}
func shellScriptFromArgv(command []string) string { return core.ShellScriptFromArgv(command) }
func shellQuote(value string) string              { return core.ShellQuote(value) }
