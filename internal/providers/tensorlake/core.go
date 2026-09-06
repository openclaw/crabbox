package tensorlake

import (
	"flag"
	"io"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type TensorlakeConfig = core.TensorlakeConfig
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
type ExitError = core.ExitError
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase
type LocalCommandRequest = core.LocalCommandRequest

const (
	providerName   = "tensorlake"
	leasePrefix    = "tlsbx_"
	namePrefix     = "crabbox-"
	defaultAPIURL  = "https://api.tensorlake.ai"
	defaultCLIPath = "tensorlake"
	targetLinux    = core.TargetLinux
	NetworkPublic  = core.NetworkPublic

	maxSandboxNameLen    = 63
	sandboxNameSuffixLen = 6
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func writeTimingJSON(w io.Writer, report timingReport) error {
	return core.WriteTimingJSON(w, report)
}

func printEnvForwardingSummary(w io.Writer, provider, behavior string, allow []string, env map[string]string) {
	core.PrintEnvForwardingSummary(w, provider, behavior, allow, env)
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

func shellQuote(s string) string {
	return core.ShellQuote(s)
}

func tensorlakeCleanupCommand(leaseID string) string {
	return "crabbox stop --provider " + providerName + " --id " + shellQuote(leaseID)
}

func cliDoctorResult(provider string, leases int, runtime string) DoctorResult {
	return core.CLIDoctorResult(provider, leases, runtime)
}
