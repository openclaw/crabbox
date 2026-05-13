package modal

import (
	"flag"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type ModalConfig = core.ModalConfig
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
type Server = core.Server
type Repo = core.Repo
type SyncManifest = core.SyncManifest
type ExitError = core.ExitError
type LocalCommandRequest = core.LocalCommandRequest
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase

const (
	providerName = "modal"
	targetLinux  = core.TargetLinux

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

func newLeaseSlug(leaseID string) string {
	return core.NewLeaseSlug(leaseID)
}

func normalizeLeaseSlug(value string) string {
	return core.NormalizeLeaseSlug(value)
}

func directLeaseLabels(cfg Config, leaseID, slug, provider, market string, keep bool, now time.Time) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, provider, market, keep, now)
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

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func handleDelegatedRunFailure(w io.Writer, req RunRequest, provider, leaseID, slug string, idleTimeout, ttl time.Duration, acquired bool, shouldStop *bool) {
	core.HandleDelegatedRunFailure(w, req, provider, leaseID, slug, idleTimeout, ttl, acquired, shouldStop)
}

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}

func shellQuote(s string) string {
	return core.ShellQuote(s)
}

func shellScriptFromArgv(command []string) string {
	return core.ShellScriptFromArgv(command)
}

func shellWords(words []string) []string {
	return core.ShellWords(words)
}

func shouldUseShell(command []string) bool {
	return core.ShouldUseShell(command)
}

func leadingEnvAssignment(command []string) bool {
	return core.LeadingEnvAssignment(command)
}

func syncExcludes(root string, cfg Config) ([]string, error) {
	return core.SyncExcludes(root, cfg)
}

func syncManifest(root string, excludes []string) (SyncManifest, error) {
	return core.BuildSyncManifest(root, excludes)
}

func checkSyncPreflight(manifest SyncManifest, cfg Config, force bool, stderr io.Writer) error {
	return core.CheckSyncPreflight(manifest, cfg, force, stderr)
}
