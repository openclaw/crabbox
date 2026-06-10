package wasi

import (
	"flag"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type WasiConfig = core.WasiConfig
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
type Server = core.Server
type Repo = core.Repo
type LeaseClaim = core.LeaseClaim
type SyncManifest = core.SyncManifest
type ExitError = core.ExitError
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase
type RunSessionHandle = core.RunSessionHandle

type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type CommandRunner = core.CommandRunner

const (
	providerName = "wasi"
	targetLinux  = core.TargetLinux
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

func shellQuote(s string) string {
	return core.ShellQuote(s)
}

func syncExcludes(root string, cfg Config) ([]string, error) {
	return core.SyncExcludes(root, cfg)
}

func syncManifest(root string, excludes []string) (SyncManifest, error) {
	return core.BuildSyncManifestFiltered(root, excludes, nil)
}

func checkSyncPreflight(manifest SyncManifest, cfg Config, force bool, stderr io.Writer) error {
	return core.CheckSyncPreflight(manifest, cfg, force, stderr)
}

func claimLeaseForRepoProvider(leaseID, slug, provider, repoRoot string, idle time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProvider(leaseID, slug, provider, repoRoot, idle, reclaim)
}

func resolveLeaseClaimForProvider(idOrSlug, provider string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(idOrSlug, provider)
}

func listLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaims()
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
}

func newLeaseID() string {
	return core.NewLeaseID()
}

func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return core.AllocateClaimLeaseSlug(leaseID, requested)
}

func writeTimingJSON(w io.Writer, r timingReport) error {
	return core.WriteTimingJSON(w, core.TimingReport(r))
}

func handleDelegatedRunFailure(w io.Writer, req RunRequest, provider, leaseID, slug string, idle, ttl time.Duration, acquired bool, shouldStop *bool) {
	core.HandleDelegatedRunFailure(w, req, provider, leaseID, slug, idle, ttl, acquired, shouldStop)
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}

func nowFrom(rt Runtime) time.Time {
	if rt.Clock != nil {
		return rt.Clock.Now()
	}
	return time.Now()
}
