package windowssandbox

import (
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type WindowsSandboxConfig = core.WindowsSandboxConfig
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
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type ExitError = core.ExitError
type SyncManifest = core.SyncManifest
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase

const (
	providerName      = "windows-sandbox"
	targetWindows     = core.TargetWindows
	windowsModeNormal = core.WindowsModeNormal
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
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

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}

func durationSecondsCeil(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	return int((duration + time.Second - 1) / time.Second)
}
