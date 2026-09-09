package wandb

import (
	"flag"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type WandbConfig = core.WandbConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type RunSessionHandle = core.RunSessionHandle
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type Server = core.Server
type LeaseClaim = core.LeaseClaim
type SSHTarget = core.SSHTarget
type Repo = core.Repo
type ExitError = core.ExitError
type FeatureSet = core.FeatureSet
type Feature = core.Feature
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type CommandRunner = core.CommandRunner
type timingReport = core.TimingReport

const (
	providerName  = "wandb"
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

func shellQuote(value string) string {
	return core.ShellQuote(value)
}

func claimWandbSandbox(leaseID, scope string, cfg Config) (LeaseClaim, error) {
	server := Server{CloudID: leaseID, Provider: providerName, Name: leaseID}
	return core.ClaimLeaseTargetForConfigScopeIfUnchanged(leaseID, leaseID, cfg, scope, server, SSHTarget{}, cfg.IdleTimeout, LeaseClaim{}, false)
}

func resolveWandbClaim(identifier string) (LeaseClaim, bool, error) {
	claim, ok, err := core.ResolveLeaseClaimForProvider(identifier, providerName)
	if err != nil || ok {
		return claim, ok, err
	}
	return core.ResolveLeaseClaimForProviderCloudID(identifier, providerName)
}

func removeWandbClaimAfter(claim LeaseClaim, action func() error) error {
	return core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, action)
}

func verifyWandbClaim(claim LeaseClaim) error {
	return core.VerifyLeaseClaimUnchanged(claim.LeaseID, claim)
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}

// handleDelegatedRunFailure delegates to the shared CLI helper that honours
// --keep-on-failure (preserves the sandbox when a run fails and the user
// asked for it). Pattern matches modal / e2b / islo / tensorlake.
func handleDelegatedRunFailure(w io.Writer, req RunRequest, provider, leaseID, slug string, idleTimeout, ttl time.Duration, acquired bool, shouldStop *bool) {
	core.HandleDelegatedRunFailure(w, req, provider, leaseID, slug, idleTimeout, ttl, acquired, shouldStop)
}

// writeTimingJSON delegates to the shared CLI helper that emits the
// machine-readable timing report on stderr. Used to satisfy the
// --timing-json contract that delegated-run sibling providers honour.
func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func timingReportWithRunResult(report timingReport, result RunResult, err error) timingReport {
	return core.TimingReportWithRunResult(report, result, err)
}

// printEnvForwardingSummary mirrors the modal/e2b/islo/tensorlake call site
// so --allow-env / env profiles / CRABBOX_ENV_ALLOW users see the redacted
// "env forwarding ..." confirmation line on stderr.
func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}
