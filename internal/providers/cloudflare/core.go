package cloudflare

import (
	"context"
	"flag"
	"io"
	"os"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type CloudflareConfig = core.CloudflareConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
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
type LeaseClaim = core.LeaseClaim
type Repo = core.Repo
type SyncManifest = core.SyncManifest
type ExitError = core.ExitError
type FeatureSet = core.FeatureSet
type Feature = core.Feature
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase

const (
	providerName  = "cloudflare"
	providerAlias = "cf"
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

func newLeaseSlug(leaseID string) string {
	return core.NewLeaseSlug(leaseID)
}

func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return core.AllocateClaimLeaseSlug(leaseID, requested)
}

func claimLeaseForRepoProvider(leaseID, slug, provider, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProvider(leaseID, slug, provider, repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProviderPondLabels(leaseID, slug, provider, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, labels map[string]string) error {
	return core.ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, provider, "", pond, repoRoot, idleTimeout, reclaim, Server{Labels: labels}, core.SSHTarget{})
}

func resolveLeaseClaimForProvider(identifier, provider string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, provider)
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
}

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func timingReportWithRunResult(report timingReport, result RunResult, err error) timingReport {
	return core.TimingReportWithRunResult(report, result, err)
}

func finalizeRunResult(result RunResult, err error) RunResult {
	return core.FinalizeRunResult(result, err)
}

func handleDelegatedRunFailure(w io.Writer, req RunRequest, provider, leaseID, slug string, idleTimeout, ttl time.Duration, acquired bool, shouldStop *bool) {
	core.HandleDelegatedRunFailure(w, req, provider, leaseID, slug, idleTimeout, ttl, acquired, shouldStop)
}

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}

func cloudflareContainerInstanceTypes() []string {
	return core.CloudflareContainerInstanceTypes()
}

func cloudflareContainerInstanceTypeForClass(class string) string {
	return core.CloudflareContainerInstanceTypeForClass(class)
}

func normalizeCloudflareContainerInstanceType(value string) (string, bool) {
	return core.NormalizeCloudflareContainerInstanceType(value)
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

func syncManifest(root string, excludes, includes []string) (SyncManifest, error) {
	return core.BuildSyncManifestFiltered(root, excludes, includes)
}

func checkSyncPreflight(manifest SyncManifest, cfg Config, force bool, stderr io.Writer) error {
	return core.CheckSyncPreflight(manifest, cfg, force, stderr)
}

func createPortableSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, tempPattern string) (*os.File, error) {
	return core.CreateSyncArchive(ctx, repo, manifest, tempPattern)
}
