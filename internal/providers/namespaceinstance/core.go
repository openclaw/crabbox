package namespaceinstance

import (
	"context"
	"flag"
	"io"
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
type SSHTarget = core.SSHTarget
type CleanupRequest = core.CleanupRequest
type LeaseClaim = core.LeaseClaim

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

var newLeaseID = core.NewLeaseID
var allocateDirectLeaseSlug = core.AllocateDirectLeaseSlug
var ensureTestboxKeyForConfig = core.EnsureTestboxKeyForConfig
var providerKeyForLease = core.ProviderKeyForLease
var sshTargetFromConfig = core.SSHTargetFromConfig
var waitForSSHReady = func(ctx context.Context, target *SSHTarget, stderr io.Writer, phase string, timeout time.Duration) error {
	return core.WaitForSSH(ctx, target, stderr)
}
var bootstrapWaitTimeout = core.BootstrapWaitTimeout
var claimLeaseTargetForConfig = core.ClaimLeaseTargetForConfig
var resolveLeaseClaimForProvider = core.ResolveLeaseClaimForProvider
var resolveLeaseClaimForProviderCloudID = core.ResolveLeaseClaimForProviderCloudID
var listLeaseClaims = core.ListLeaseClaims
var removeLeaseClaim = core.RemoveLeaseClaim
var removeStoredTestboxKey = core.RemoveStoredTestboxKey
var useStoredTestboxKey = core.UseStoredTestboxKey
var directLeaseLabels = core.DirectLeaseLabels
var touchDirectLeaseLabels = core.TouchDirectLeaseLabels
var shouldCleanupServer = core.ShouldCleanupServer

func leaseLabelTime(t time.Time) string {
	return core.LeaseLabelTime(t)
}
