package azuredynamicsessions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type AzureDynamicSessionsConfig = core.AzureDynamicSessionsConfig
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
type SyncManifest = core.SyncManifest
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type ExitError = core.ExitError
type LeaseClaim = core.LeaseClaim
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase

const (
	providerName = "azure-dynamic-sessions"
	targetLinux  = core.TargetLinux

	networkPublic = core.NetworkPublic

	dynamicSessionsAudience = "https://dynamicsessions.io"
	tokenEnvName            = "CRABBOX_AZURE_DYNAMIC_SESSIONS_TOKEN"
	exitSentinel            = "__crabbox_azds_exit_code__="
)

type statusView = core.StatusView

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func validateNativeCredentialDestination(cfg Config) error {
	return core.ValidateNativeCredentialDestination(cfg, providerName)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
}

func newSessionID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "azds-" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000"), ".", "")
	}
	return "azds-" + hex.EncodeToString(b[:])
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

func claimLeaseForRepoProviderScope(leaseID, slug, provider, providerScope, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProviderScope(leaseID, slug, provider, providerScope, repoRoot, idleTimeout, reclaim)
}

func resolveLeaseClaimForProvider(identifier, provider string) (core.LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, provider)
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
}

func listLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaims()
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

func shellQuote(value string) string {
	return core.ShellQuote(value)
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

func summarizeJSON(data []byte) string {
	return core.SummarizeJSON(data)
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}

func delegatedSyncOptionsError(spec ProviderSpec, req RunRequest) error {
	return core.RejectDelegatedSyncOptionsForSpec(spec, req)
}

func durationSecondsCeil(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	return int((duration + time.Second - 1) / time.Second)
}

func durationMillisecondsCeil(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return int64((duration + time.Millisecond - 1) / time.Millisecond)
}

func providerError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %w", providerName, action, err)
}
