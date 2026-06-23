package cloudflaredynamicworkers

import (
	"flag"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type CloudflareDynamicWorkersConfig = core.CloudflareDynamicWorkersConfig
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
type RunScriptSpec = core.RunScriptSpec
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type CleanupRequest = core.CleanupRequest
type Server = core.Server
type LeaseClaim = core.LeaseClaim
type Repo = core.Repo
type ExitError = core.ExitError
type timingReport = core.TimingReport

const (
	providerName             = "cloudflare-dynamic-workers"
	targetWorker             = core.TargetWorkerRuntime
	defaultCompatibilityDate = core.DefaultCloudflareDynamicWorkersCompatibilityDate
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

func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return core.AllocateClaimLeaseSlug(leaseID, requested)
}

func claimLease(leaseID, slug string, cfg Config, repoRoot string, idleTimeout time.Duration, reclaim bool, server Server) error {
	scope, err := loaderClaimScope(cfg)
	if err != nil {
		return err
	}
	return core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, scope, cfg.Pond, repoRoot, idleTimeout, reclaim, server, core.SSHTarget{TargetOS: targetWorker})
}

func resolveLeaseClaim(identifier string, cfg Config) (core.LeaseClaim, bool, error) {
	scope, err := loaderClaimScope(cfg)
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	if identifier != "" {
		exact, exists, err := core.ReadLeaseClaimWithPresence(identifier)
		if err != nil {
			return core.LeaseClaim{}, false, err
		}
		if exists {
			if exact.Provider == providerName && exact.ProviderScope == scope {
				return exact, true, nil
			}
			if exact.Provider == providerName {
				return core.LeaseClaim{}, false, exit(2, "%s claim %s belongs to a different loader endpoint", providerName, identifier)
			}
		}
	}
	claims, err := core.ListLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	var matched core.LeaseClaim
	for _, claim := range claims {
		if claim.Provider != providerName || claim.ProviderScope != scope {
			continue
		}
		if claim.LeaseID == identifier {
			return claim, true, nil
		}
		if matched.LeaseID == "" && core.LeaseClaimMatchesIdentifier(claim, identifier) {
			matched = claim
		}
	}
	return matched, matched.LeaseID != "", nil
}

func listLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaims()
}

func removeLeaseClaimIfUnchanged(leaseID string, expected LeaseClaim) error {
	return core.RemoveLeaseClaimIfUnchanged(leaseID, expected)
}

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func timingReportWithRunResult(report timingReport, result RunResult, err error) timingReport {
	return core.TimingReportWithRunResult(report, result, err)
}

func timingReportWithProviderError(report timingReport) timingReport {
	report.RunStatus = core.RunStatusFailed
	report.ErrorKind = core.RunErrorProvider
	return report
}

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}

func rejectDelegatedSyncOptions(spec ProviderSpec, req RunRequest) error {
	return core.RejectDelegatedSyncOptionsForSpec(spec, req)
}

func now(rt Runtime) time.Time {
	if rt.Clock != nil {
		return rt.Clock.Now()
	}
	return time.Now()
}
