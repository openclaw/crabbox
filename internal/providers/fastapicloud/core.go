package fastapicloud

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type FastAPICloudConfig = core.FastAPICloudConfig
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
type ExitError = core.ExitError

const (
	providerName = "fastapi-cloud"
	targetLinux  = core.TargetLinux

	networkPublic = core.NetworkPublic
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}
