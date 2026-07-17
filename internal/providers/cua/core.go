package cua

import (
	"flag"
	"time"

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
type Server = core.Server
type Repo = core.Repo
type LeaseClaim = core.LeaseClaim
type ExitError = core.ExitError
type timingReport = core.TimingReport
type timingPhase = core.TimingPhase

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
	cuaTrackingIssue         = "https://github.com/openclaw/crabbox/issues/381"
	maxBridgeTimeoutSeconds  = int64((1<<63 - 1) / int64(time.Second))
)

func provisioningUnsupported() error {
	return exit(2, "provider=cua provisioning is disabled: the upstream CUA create API has no idempotency key or client-assigned identity echoed by create/list/get, so a timed-out create could orphan a billed sandbox; use doctor, list, or status; tracking issue: %s", cuaTrackingIssue)
}

func mutationUnsupported() error {
	return exit(2, "provider=cua is experimental and read-only: remote mutation is disabled because upstream deletion cannot atomically bind to an immutable sandbox identity; use doctor, list, or status; tracking issue: %s", cuaTrackingIssue)
}

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
}

func newLeaseSlug(leaseID string) string {
	return core.NewLeaseSlug(leaseID)
}

func listCUALeaseClaims() ([]LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}
