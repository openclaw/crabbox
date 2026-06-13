package nvidiabrev

import (
	"flag"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type NvidiaBrevConfig = core.NvidiaBrevConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type AcquireRequest = core.AcquireRequest
type ResolveRequest = core.ResolveRequest
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type ReleaseLeaseRequest = core.ReleaseLeaseRequest
type TouchRequest = core.TouchRequest
type CleanupRequest = core.CleanupRequest
type LeaseTarget = core.LeaseTarget
type Server = core.Server
type TailscaleConfig = core.TailscaleConfig
type Feature = core.Feature
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult

const (
	providerName = "nvidia-brev"
	targetLinux  = core.TargetLinux
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

func cliDoctorResult(provider string, leases int, runtime string) DoctorResult {
	return core.CLIDoctorResult(provider, leases, runtime)
}

func isNvidiaBrevProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "brev", "nvidia":
		return true
	default:
		return false
	}
}
