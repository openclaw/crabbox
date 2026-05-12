package tensorlake

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
type TensorlakeConfig = core.TensorlakeConfig
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
type ExitError = core.ExitError
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase
type LocalCommandRequest = core.LocalCommandRequest

const (
	providerName    = "tensorlake"
	leasePrefix     = "tlsbx_"
	namePrefix      = "crabbox-"
	defaultAPIURL   = "https://api.tensorlake.ai"
	defaultCLIPath  = "tensorlake"
	defaultCPUs     = 1
	defaultMemoryMB = 1024
	defaultDiskMB   = 10240
	defaultWorkdir  = "/workspace"
	targetLinux     = core.TargetLinux
	NetworkPublic   = core.NetworkPublic
	statusViewReady = "running"

	maxSandboxNameLen    = 63
	sandboxNameSuffixLen = 6
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}

func handleDelegatedRunFailure(w io.Writer, req RunRequest, provider, leaseID, slug string, idleTimeout, ttl time.Duration, acquired bool, shouldStop *bool) {
	core.HandleDelegatedRunFailure(w, req, provider, leaseID, slug, idleTimeout, ttl, acquired, shouldStop)
}

func newLeaseSlug(leaseID string) string {
	return core.NewLeaseSlug(leaseID)
}

func normalizeLeaseSlug(value string) string {
	return core.NormalizeLeaseSlug(value)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
}

func claimLeaseForRepoProvider(leaseID, slug, provider, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProvider(leaseID, slug, provider, repoRoot, idleTimeout, reclaim)
}

func resolveLeaseClaim(identifier string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaim(identifier)
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
}

func shouldUseShell(command []string) bool {
	return core.ShouldUseShell(command)
}

func shellScriptFromArgv(command []string) string {
	return core.ShellScriptFromArgv(command)
}

func shellQuote(s string) string {
	return core.ShellQuote(s)
}

func syncExcludes(root string, cfg Config) ([]string, error) {
	return core.SyncExcludes(root, cfg)
}

func syncManifest(root string, excludes []string) (core.SyncManifest, error) {
	return core.BuildSyncManifest(root, excludes)
}

func checkSyncPreflight(manifest core.SyncManifest, cfg Config, force bool, stderr io.Writer) error {
	return core.CheckSyncPreflight(manifest, cfg, force, stderr)
}

type SyncManifest = core.SyncManifest
