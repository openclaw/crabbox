package cloudflaresandbox

import (
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type CloudflareSandboxConfig = core.CloudflareSandboxConfig
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
type timingPhase = core.TimingPhase

const (
	providerName   = "cloudflare-sandbox"
	providerFamily = "cloudflare"
	leasePrefix    = "cfsbx_"
	targetLinux    = core.TargetLinux
	NetworkPublic  = "public"
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
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

func claimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, server Server) error {
	return core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim, server, core.SSHTarget{})
}

func readLeaseClaim(leaseID string) (LeaseClaim, error) {
	return core.ReadLeaseClaim(leaseID)
}

func listCloudflareSandboxLeaseClaims() ([]core.LeaseClaim, error) {
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

func cloudflareSandboxCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " --id " + shellQuote(leaseID)
}

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}
