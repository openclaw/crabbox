package agentsandbox

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type AgentSandboxConfig = core.AgentSandboxConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type DoctorCheck = core.DoctorCheck
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type CleanupRequest = core.CleanupRequest
type LeaseClaim = core.LeaseClaim
type Repo = core.Repo
type Server = core.Server
type ExitError = core.ExitError
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase
type SyncManifest = core.SyncManifest
type CommandRunner = core.CommandRunner
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult

const (
	providerName = "agent-sandbox"
	leasePrefix  = "asbx_"
	namePrefix   = "crabbox-"

	agentSandboxCoreGroupVersion       = "agents.x-k8s.io/v1beta1"
	agentSandboxExtensionsGroupVersion = "extensions.agents.x-k8s.io/v1beta1"

	sandboxResource      = "sandboxes"
	sandboxClaimResource = "sandboxclaims"
	warmPoolResource     = "sandboxwarmpools"
	podResource          = "pods"
	targetLinux          = core.TargetLinux
	networkPublic        = core.NetworkPublic
	statusViewReady      = "running"

	agentSandboxCleanupTimeout = 15 * time.Second
	agentSandboxStatusPoll     = 2 * time.Second
	agentSandboxClaimUIDLabel  = "agents.x-k8s.io/claim-uid"
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func expandUserPath(path string) string {
	return core.ExpandUserPath(path)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
}

func newLeaseSlug(leaseID string) string {
	return core.NewLeaseSlug(leaseID)
}

func claimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return core.ClaimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim)
}

func readLeaseClaim(leaseID string) (LeaseClaim, error) {
	return core.ReadLeaseClaim(leaseID)
}

func writeTimingJSON(w io.Writer, report core.TimingReport) error {
	return core.WriteTimingJSON(w, report)
}

func timingReportWithRunResult(report core.TimingReport, result RunResult, err error) core.TimingReport {
	return core.TimingReportWithRunResult(report, result, err)
}

func timingReportWithProviderError(report core.TimingReport) core.TimingReport {
	report.RunStatus = core.RunStatusFailed
	report.ErrorKind = core.RunErrorProvider
	return report
}

func handleDelegatedRunFailure(w io.Writer, cfg Config, req RunRequest, leaseID, slug string, acquired bool, shouldStop *bool) {
	if !req.KeepOnFailure {
		return
	}
	if acquired && !req.Keep && shouldStop != nil {
		*shouldStop = false
	}
	id := slug
	if id == "" {
		id = leaseID
	}
	fmt.Fprintf(w, "keep-on-failure: kept lease=%s slug=%s expires=idle/ttl idle_timeout=%s ttl=%s\n", leaseID, blank(slug, "-"), cfg.IdleTimeout, cfg.TTL)
	fmt.Fprintf(w, "rerun: %s --id %s -- <command>\n", agentSandboxRecoveryCommand(cfg, "run"), shellQuote(id))
	fmt.Fprintf(w, "stop: %s %s\n", agentSandboxRecoveryCommand(cfg, "stop"), shellQuote(id))
}

func agentSandboxRecoveryCommand(cfg Config, command string) string {
	args := []string{
		"crabbox", command,
		"--provider", providerName,
		"--agent-sandbox-kubectl", cfg.AgentSandbox.Kubectl,
	}
	if cfg.AgentSandbox.Kubeconfig != "" {
		args = append(args, "--agent-sandbox-kubeconfig", cfg.AgentSandbox.Kubeconfig)
	}
	args = append(args,
		"--agent-sandbox-context", cfg.AgentSandbox.Context,
		"--agent-sandbox-namespace", cfg.AgentSandbox.Namespace,
		"--agent-sandbox-warm-pool", cfg.AgentSandbox.WarmPool,
	)
	if cfg.AgentSandbox.Container != "" {
		args = append(args, "--agent-sandbox-container", cfg.AgentSandbox.Container)
	}
	args = append(args, "--agent-sandbox-workdir", cfg.AgentSandbox.Workdir)
	words := make([]string, 0, len(args)+1)
	if cfg.AgentSandbox.Kubeconfig == "" {
		if kubeconfig := strings.TrimSpace(os.Getenv("KUBECONFIG")); kubeconfig != "" {
			words = append(words, "KUBECONFIG="+shellQuote(kubeconfig))
		}
	}
	for _, arg := range args {
		words = append(words, shellQuote(arg))
	}
	return strings.Join(words, " ")
}

func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return core.AllocateDirectLeaseSlug(leaseID, requested, nil)
}

func resolveLeaseClaimForProvider(identifier, provider string) (LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, provider)
}

func listLeaseClaimsWithPrefix(prefix string) ([]LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(prefix)
}

func removeLeaseClaimIfUnchanged(leaseID string, expected LeaseClaim) error {
	return core.RemoveLeaseClaimIfUnchanged(leaseID, expected)
}

func updateLeaseClaimLabelsIfUnchanged(leaseID string, expected LeaseClaim, labels map[string]string) (LeaseClaim, error) {
	return core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, expected, labels)
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

func syncExcludes(root string, cfg Config) ([]string, error) {
	return core.SyncExcludes(root, cfg)
}

func syncManifest(root string, excludes, includes []string) (core.SyncManifest, error) {
	return core.BuildSyncManifestFiltered(root, excludes, includes)
}

func checkSyncPreflight(manifest core.SyncManifest, cfg Config, force bool, stderr io.Writer) error {
	return core.CheckSyncPreflight(manifest, cfg, force, stderr)
}

func createPortableSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, tempPattern string) (*os.File, error) {
	return core.CreateSyncArchive(ctx, repo, manifest, tempPattern)
}
