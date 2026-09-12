package e2b

import (
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type E2BConfig = core.E2BConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type RunSessionHandle = core.RunSessionHandle
type LeaseClaim = core.LeaseClaim
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type Server = core.Server
type SSHTarget = core.SSHTarget
type Repo = core.Repo
type ExitError = core.ExitError
type timingPhase = core.TimingPhase

const (
	e2bProvider = "e2b"
	targetLinux = core.TargetLinux

	NetworkPublic = core.NetworkPublic
)

type statusView = core.StatusView

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
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

func directLeaseLabels(cfg Config, leaseID, slug, provider, market string, keep bool, now time.Time) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, provider, market, keep, now)
}

var claimLeaseForRepoProvider = core.ClaimLeaseForRepoProvider

var claimLeaseTargetForRepoConfig = core.ClaimLeaseTargetForRepoConfig

var claimLeaseTargetForRepoConfigIfUnchanged = core.ClaimLeaseTargetForRepoConfigIfUnchanged

var claimLeaseTargetForConfigIfUnchanged = core.ClaimLeaseTargetForConfigIfUnchanged

func resolveLeaseClaimForProviderScopeWithExact(identifier, providerScope string) (LeaseClaim, bool, bool, error) {
	return core.ResolveLeaseClaimForProviderScopeWithExact(identifier, e2bProvider, providerScope)
}

func resolveLeaseClaimForProviderCloudIDScope(cloudID, providerScope string) (LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProviderCloudIDScope(cloudID, e2bProvider, providerScope)
}

func readLeaseClaimWithPresence(leaseID string) (LeaseClaim, bool, error) {
	return core.ReadLeaseClaimWithPresence(leaseID)
}

func removeLeaseClaimIfUnchangedAfter(leaseID string, expected LeaseClaim, action func() error) error {
	return core.RemoveLeaseClaimIfUnchangedAfter(leaseID, expected, action)
}

func providerClaimScope(cfg Config) string {
	return core.ProviderClaimScope(e2bProvider, cfg)
}

func isCanonicalLeaseID(value string) bool {
	return core.IsCanonicalLeaseID(value)
}

func resolveLeaseClaim(identifier string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaim(identifier)
}

func shellQuote(s string) string {
	return core.ShellQuote(s)
}

func summarizeJSON(data []byte) string {
	return core.SummarizeJSON(data)
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}
