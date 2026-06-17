package cloudflaresandbox

import (
	"flag"
	"io"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type CloudflareSandboxConfig = core.CloudflareSandboxConfig
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
type ExitError = core.ExitError

const (
	providerName   = "cloudflare-sandbox"
	providerFamily = "cloudflare"
	defaultWorkdir = "/workspace/crabbox"
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func writeTimingJSON(w io.Writer, report core.TimingReport) error {
	return core.WriteTimingJSON(w, report)
}
