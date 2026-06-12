package mxc

import (
	"io"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type LeaseOptions = core.LeaseOptions
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type timingReport = core.TimingReport

const (
	providerName  = "mxc"
	targetWindows = core.TargetWindows
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}
func writeTimingJSON(w io.Writer, report timingReport) error { return core.WriteTimingJSON(w, report) }
func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
}
