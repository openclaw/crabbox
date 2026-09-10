package vast

import (
	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type VastConfig = core.VastConfig
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
type LeaseTarget = core.LeaseTarget
type Server = core.Server
type SSHTarget = core.SSHTarget

const (
	providerName = "vast"
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func markVastWorkRootExplicit(cfg *Config) {
	core.MarkVastWorkRootExplicit(cfg)
}

func markReleaseActionExplicit(cfg *Config) {
	core.MarkDeleteOnReleaseExplicit(cfg, providerName)
}

func normalizeInstanceType(value string) string {
	return core.NormalizeVastInstanceType(value)
}
