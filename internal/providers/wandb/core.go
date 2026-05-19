package wandb

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type WandbConfig = core.WandbConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type Backend = core.Backend
type WarmupRequest = core.WarmupRequest
type RunRequest = core.RunRequest
type RunResult = core.RunResult
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type StatusRequest = core.StatusRequest
type StatusView = core.StatusView
type StopRequest = core.StopRequest
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type Server = core.Server
type Repo = core.Repo
type ExitError = core.ExitError
type FeatureSet = core.FeatureSet
type Feature = core.Feature
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type CommandRunner = core.CommandRunner

const (
	providerName  = "wandb"
	targetLinux   = core.TargetLinux
	networkPublic = core.NetworkPublic
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
