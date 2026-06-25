package replicate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type ReplicateConfig = core.ReplicateConfig
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
type Server = core.Server
type Repo = core.Repo
type SyncManifest = core.SyncManifest
type LeaseClaim = core.LeaseClaim
type ExitError = core.ExitError
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase

const (
	providerName              = "replicate"
	defaultAPIURL             = "https://api.replicate.com/v1"
	defaultWorkdir            = "/workspace/crabbox"
	defaultWaitSecs           = 0
	defaultPollIntervalSecs   = 2
	defaultExecTimeoutSecs    = 3600
	defaultCancelAfterSecs    = 0
	defaultMaxArchiveBytes    = 10 * 1024 * 1024
	envCrabboxReplicateToken  = "CRABBOX_REPLICATE_API_TOKEN"
	envReplicateToken         = "REPLICATE_API_TOKEN"
	leasePrefix               = "rbx_"
	targetLinux               = core.TargetLinux
	networkPublic             = core.NetworkPublic
	statusViewReady           = "succeeded"
	runnerExitCodeJSONName    = "exit_code"
	runnerExitCodeAltJSONName = "exitCode"
)

type RunnerInput struct {
	Command      []string          `json:"command"`
	Workdir      string            `json:"workdir"`
	ArchiveURL   string            `json:"archive_url,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	TimeoutSecs  int               `json:"timeout_secs,omitempty"`
	CancelAfter  int               `json:"cancel_after_secs,omitempty"`
	MaxLogBytes  int               `json:"max_log_bytes,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	OutputSchema string            `json:"output_schema,omitempty"`
}

type RunnerOutput struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

func DefaultConfig() ReplicateConfig {
	return ReplicateConfig{
		APIURL:           defaultAPIURL,
		Workdir:          defaultWorkdir,
		WaitSecs:         defaultWaitSecs,
		PollIntervalSecs: defaultPollIntervalSecs,
		ExecTimeoutSecs:  defaultExecTimeoutSecs,
		CancelAfterSecs:  defaultCancelAfterSecs,
		MaxArchiveBytes:  defaultMaxArchiveBytes,
	}
}

func ResolveAPIToken() (string, string, bool) {
	if token := strings.TrimSpace(os.Getenv(envCrabboxReplicateToken)); token != "" {
		return token, envCrabboxReplicateToken, true
	}
	if token := strings.TrimSpace(os.Getenv(envReplicateToken)); token != "" {
		return token, envReplicateToken, true
	}
	return "", "", false
}

func ValidateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.Provider) != providerName {
		return nil
	}
	deployment := strings.TrimSpace(cfg.Replicate.Deployment)
	version := strings.TrimSpace(cfg.Replicate.Version)
	if deployment != "" && version != "" {
		return core.Exit(2, "provider=replicate accepts exactly one of replicate.deployment or replicate.version, not both")
	}
	if cfg.Replicate.WaitSecs < 0 {
		return core.Exit(2, "replicate waitSecs must be non-negative")
	}
	if cfg.Replicate.PollIntervalSecs < 0 {
		return core.Exit(2, "replicate pollIntervalSecs must be non-negative")
	}
	if cfg.Replicate.ExecTimeoutSecs < 0 {
		return core.Exit(2, "replicate execTimeoutSecs must be non-negative")
	}
	if cfg.Replicate.CancelAfterSecs < 0 {
		return core.Exit(2, "replicate cancelAfterSecs must be non-negative")
	}
	if cfg.Replicate.MaxArchiveBytes < 0 {
		return core.Exit(2, "replicate maxArchiveBytes must be non-negative")
	}
	return nil
}

func validateRunnerTargetConfig(cfg Config) error {
	deployment := strings.TrimSpace(cfg.Replicate.Deployment)
	version := strings.TrimSpace(cfg.Replicate.Version)
	if deployment == "" && version == "" {
		return core.Exit(2, "provider=replicate requires exactly one of replicate.deployment or replicate.version")
	}
	if deployment != "" && version != "" {
		return core.Exit(2, "provider=replicate accepts exactly one of replicate.deployment or replicate.version, not both")
	}
	return nil
}

func ParseRunnerOutput(data []byte) (RunnerOutput, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return RunnerOutput{}, fmt.Errorf("decode replicate runner output: %w", err)
	}
	exitRaw, ok := raw[runnerExitCodeJSONName]
	if !ok {
		exitRaw, ok = raw[runnerExitCodeAltJSONName]
	}
	if !ok {
		return RunnerOutput{}, fmt.Errorf("replicate runner output missing required exit_code")
	}
	var exitCode int
	if err := json.Unmarshal(exitRaw, &exitCode); err != nil {
		return RunnerOutput{}, fmt.Errorf("replicate runner output exit_code must be an integer: %w", err)
	}
	var out RunnerOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return RunnerOutput{}, fmt.Errorf("decode replicate runner output fields: %w", err)
	}
	out.ExitCode = exitCode
	return out, nil
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func timingReportWithRunResult(report timingReport, result RunResult, err error) timingReport {
	return core.TimingReportWithRunResult(report, result, err)
}

func handleDelegatedRunFailure(w io.Writer, req RunRequest, provider, leaseID, slug string, idleTimeout, ttl time.Duration, acquired bool, shouldStop *bool) {
	core.HandleDelegatedRunFailure(w, req, provider, leaseID, slug, idleTimeout, ttl, acquired, shouldStop)
}

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}

func rejectDelegatedSyncOptionsForSpec(spec ProviderSpec, req RunRequest) error {
	return core.RejectDelegatedSyncOptionsForSpec(spec, req)
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

func coreCreateSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, tempPattern string) (*os.File, error) {
	return core.CreateSyncArchive(ctx, repo, manifest, tempPattern)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
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

func readLeaseClaim(leaseID string) (core.LeaseClaim, error) {
	return core.ReadLeaseClaim(leaseID)
}

func listReplicateLeaseClaims() ([]core.LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
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

func replicateEndpointScope(baseURL string) string {
	digest := sha256.Sum256([]byte(baseURL))
	return "replicate-endpoint-sha256:" + hex.EncodeToString(digest[:])
}
