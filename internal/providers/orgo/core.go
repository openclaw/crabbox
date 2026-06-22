package orgo

import (
	"flag"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type OrgoConfig = core.OrgoConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type Server = core.Server
type Repo = core.Repo
type LeaseClaim = core.LeaseClaim
type ExitError = core.ExitError
type timingReport = core.TimingReport

const (
	providerName  = "orgo"
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

func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return core.AllocateClaimLeaseSlug(leaseID, requested)
}

func claimLeaseForRepoProviderEndpoint(leaseID, slug, repoRoot string, idleTimeout time.Duration, reclaim bool, server Server) error {
	return core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, providerName, "", "", repoRoot, idleTimeout, reclaim, server, core.SSHTarget{TargetOS: targetLinux})
}

func resolveLeaseClaimForProvider(identifier string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, providerName)
}

func resolveLeaseClaimForProviderCloudID(cloudID string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProviderCloudID(cloudID, providerName)
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

func shellScriptFromArgv(command []string) string {
	return core.ShellScriptFromArgv(command)
}

func shellQuote(value string) string {
	return core.ShellQuote(value)
}

func rejectDelegatedSyncOptionsForSpec(spec ProviderSpec, req RunRequest) error {
	return core.RejectDelegatedSyncOptionsForSpec(spec, req)
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}
