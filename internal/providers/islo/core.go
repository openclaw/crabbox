package islo

import (
	"flag"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type statusView = core.StatusView

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func rejectDelegatedSyncOptions(provider string, req RunRequest) error {
	return core.RejectDelegatedSyncOptions(provider, req)
}

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func printKeepOnFailureDelegatedHint(w io.Writer, provider, leaseID, slug string, idleTimeout, ttl time.Duration) {
	core.PrintKeepOnFailureDelegatedHint(w, provider, leaseID, slug, idleTimeout, ttl)
}

func handleDelegatedRunFailure(w io.Writer, req RunRequest, provider, leaseID, slug string, idleTimeout, ttl time.Duration, acquired bool, shouldStop *bool) {
	core.HandleDelegatedRunFailure(w, req, provider, leaseID, slug, idleTimeout, ttl, acquired, shouldStop)
}

func shouldUseShell(command []string) bool {
	return core.ShouldUseShell(command)
}

func shellScriptFromArgv(command []string) string {
	return core.ShellScriptFromArgv(command)
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

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
}

func claimLeaseForRepoProvider(leaseID, slug, provider, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProvider(leaseID, slug, provider, repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProviderWithCrew(leaseID, slug, provider, crew, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProviderWithCrew(leaseID, slug, provider, crew, repoRoot, idleTimeout, reclaim)
}

func resolveLeaseClaim(identifier string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaim(identifier)
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
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

func shellQuote(s string) string {
	return core.ShellQuote(s)
}
