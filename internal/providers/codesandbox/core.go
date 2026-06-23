package codesandbox

import (
	"flag"
	"io"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type CodeSandboxConfig = core.CodeSandboxConfig
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
type CleanupRequest = core.CleanupRequest
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type PauseRequest = core.PauseRequest
type ResumeRequest = core.ResumeRequest
type PortsRequest = core.PortsRequest
type Server = core.Server
type Repo = core.Repo
type LeaseClaim = core.LeaseClaim
type ExitError = core.ExitError
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult

const (
	providerName         = "codesandbox"
	providerFamily       = "codesandbox"
	leasePrefix          = "csbx_"
	defaultWorkdir       = "/project/workspace"
	defaultBridgeCommand = "node"
	defaultSDKPackage    = "@codesandbox/sdk@2.4.2"
	targetLinux          = core.TargetLinux
	NetworkPublic        = core.NetworkPublic

	codesandboxPrimaryAPIKeyEnv  = "CRABBOX_CODESANDBOX_API_KEY"
	codesandboxFallbackAPIKeyEnv = "CSB_API_KEY"
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}

func delegatedSyncOptionsError(spec ProviderSpec, req RunRequest) error {
	return core.RejectDelegatedSyncOptionsForSpec(spec, req)
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

func readLeaseClaim(leaseID string) (LeaseClaim, error) {
	return core.ReadLeaseClaim(leaseID)
}

func listCodeSandboxLeaseClaims() ([]LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

func removeLeaseClaim(leaseID string) {
	core.RemoveLeaseClaim(leaseID)
}

func removeLeaseClaimIfUnchanged(leaseID string, expected LeaseClaim) error {
	return core.RemoveLeaseClaimIfUnchanged(leaseID, expected)
}

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
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

func codeSandboxCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " " + shellQuote(leaseID)
}

func operationTimeout(cfg CodeSandboxConfig) time.Duration {
	seconds := cfg.OperationTimeoutSecs
	if seconds <= 0 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

func bridgeCommand(cfg CodeSandboxConfig) string {
	if command := strings.TrimSpace(cfg.BridgeCommand); command != "" {
		return command
	}
	return defaultBridgeCommand
}

func sdkPackage(cfg CodeSandboxConfig) string {
	if pkg := strings.TrimSpace(cfg.SDKPackage); pkg != "" {
		return pkg
	}
	return defaultSDKPackage
}

func doctorListLimit(cfg CodeSandboxConfig) int {
	if cfg.DoctorListLimit <= 0 {
		return 1
	}
	return cfg.DoctorListLimit
}

func authFromEnv() (string, string, bool) {
	if token := strings.TrimSpace(os.Getenv(codesandboxPrimaryAPIKeyEnv)); token != "" {
		return token, codesandboxPrimaryAPIKeyEnv, true
	}
	if token := strings.TrimSpace(os.Getenv(codesandboxFallbackAPIKeyEnv)); token != "" {
		return token, codesandboxFallbackAPIKeyEnv, true
	}
	return "", "", false
}

func redactToken(text, token string) string {
	if token = strings.TrimSpace(token); token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "[redacted]")
}

func doctorCheck(name string, err error, details map[string]string) DoctorCheck {
	if err != nil {
		return DoctorCheck{Status: "error", Check: name, Message: err.Error(), Details: details}
	}
	return DoctorCheck{Status: "ok", Check: name, Message: "ready", Details: details}
}

func discardRuntime() Runtime {
	return Runtime{Stdout: io.Discard, Stderr: io.Discard}
}
