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
	providerName = "cloudflare-dynamic-workers"
	targetWorker = core.TargetWorkerRuntime
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
	return core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, "", cfg.Pond, repoRoot, idleTimeout, reclaim, server, core.SSHTarget{TargetOS: targetWorker})
}

func resolveLeaseClaim(identifier string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, providerName)
}

func listLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaims()
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
}

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func handleDelegatedRunFailure(w io.Writer, req RunRequest, provider, leaseID, slug string, idleTimeout, ttl time.Duration, acquired bool, shouldStop *bool) {
	core.HandleDelegatedRunFailure(w, req, provider, leaseID, slug, idleTimeout, ttl, acquired, shouldStop)
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
