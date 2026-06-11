package sandboxruntime

import (
	"io"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type SandboxRuntimeConfig = core.SandboxRuntimeConfig
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
type Repo = core.Repo
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type timingReport = core.TimingReport

const (
	providerName   = "sandbox-runtime"
	providerFamily = "sandbox-runtime"
	defaultCLIPath = "srt"
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}

func shouldUseShell(command []string) bool {
	return core.ShouldUseShell(command)
}

func leadingEnvAssignment(command []string) bool {
	return core.LeadingEnvAssignment(command)
}

func shellScriptFromArgv(command []string) string {
	return core.ShellScriptFromArgv(command)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
}
