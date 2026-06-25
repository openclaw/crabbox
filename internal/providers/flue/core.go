package flue

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type FlueConfig = core.FlueConfig
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

const (
	providerName       = "flue"
	providerKind       = "delegated-run"
	defaultCLIPath     = "flue"
	defaultWorkflow    = "crabbox-runner"
	defaultTarget      = "node"
	defaultWorkdir     = "/workspace/crabbox"
	defaultTimeoutSecs = 1800
	protocolVersion    = 1
	operationRun       = "run"
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}
