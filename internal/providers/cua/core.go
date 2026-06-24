package cua

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type CuaConfig = core.CuaConfig
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
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type Repo = core.Repo
type LeaseClaim = core.LeaseClaim

const (
	providerName             = "cua"
	defaultImage             = "ubuntu:24.04"
	defaultKind              = "container"
	defaultRegion            = ""
	defaultWorkdir           = "/workspace/crabbox"
	defaultBridgeCommand     = "python3"
	defaultSDKPackage        = "cua"
	defaultSDKImport         = "cua"
	defaultSDKFallbackImport = "cua_sandbox"
	targetLinux              = core.TargetLinux
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
