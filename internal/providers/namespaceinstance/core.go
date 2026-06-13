package namespaceinstance

import (
	"context"
	"flag"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type NamespaceInstanceConfig = core.NamespaceInstanceConfig
type ProviderSpec = core.ProviderSpec
type Runtime = core.Runtime
type CommandRunner = core.CommandRunner
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type Backend = core.Backend
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type DoctorCheck = core.DoctorCheck
type AcquireRequest = core.AcquireRequest
type ResolveRequest = core.ResolveRequest
type ReleaseLeaseRequest = core.ReleaseLeaseRequest
type TouchRequest = core.TouchRequest
type ListRequest = core.ListRequest
type LeaseView = core.LeaseView
type LeaseTarget = core.LeaseTarget
type Server = core.Server

const (
	providerName       = "namespace-instance"
	providerAlias      = "namespace-compute"
	defaultMachineType = "linux-small"
	defaultWorkRoot    = "/work/crabbox"
	targetLinux        = core.TargetLinux
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func normalizeLeaseSlug(value string) string {
	return core.NormalizeLeaseSlug(value)
}

var contextWithTimeout = context.WithTimeout

func commandTimeout() time.Duration {
	return 30 * time.Second
}
