package githubcodespaces

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type GitHubCodespacesConfig = core.GitHubCodespacesConfig
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
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult

const (
	providerName               = "github-codespaces"
	providerFamily             = "github-codespaces"
	defaultGHPath              = "gh"
	defaultWorkRoot            = "/workspaces/crabbox"
	defaultSSHConfigFileMode   = 0o600
	defaultAPIURL              = "https://api.github.com"
	defaultCodespaceMachine    = "basicLinux32gb"
	defaultIdleTimeoutMinutes  = 30
	defaultRetentionPeriodDays = 7
	targetLinux                = core.TargetLinux
	networkPublic              = core.NetworkPublic
	defaultSSHPort             = "22"
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func markDeleteOnReleaseExplicit(cfg *Config) {
	core.MarkDeleteOnReleaseExplicit(cfg, providerName)
}

func deleteOnReleaseExplicit(cfg Config) bool {
	return core.DeleteOnReleaseExplicit(cfg, providerName)
}

func blank(value, fallback string) string {
	return core.Blank(value, fallback)
}
