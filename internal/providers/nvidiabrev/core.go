package nvidiabrev

import (
	"flag"
	"strings"
	"time"

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
type Repo = core.Repo
type TailscaleConfig = core.TailscaleConfig
type Feature = core.Feature
type LocalCommandRequest = core.LocalCommandRequest
type LocalCommandResult = core.LocalCommandResult
type LeaseClaim = core.LeaseClaim
type SSHTarget = core.SSHTarget

const (
	providerName   = "nvidia-brev"
	targetLinux    = core.TargetLinux
	networkPublic  = core.NetworkPublic
	defaultSSHPort = "22"
)

func exit(code int, format string, args ...any) core.ExitError {
	return core.Exit(code, format, args...)
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	return core.FlagWasSet(fs, name)
}

func releaseActionExplicit(cfg Config) bool {
	return core.DeleteOnReleaseExplicit(cfg, providerName)
}

func markReleaseActionExplicit(cfg *Config) {
	core.MarkDeleteOnReleaseExplicit(cfg, providerName)
}

func nvidiaBrevWorkRootExplicit(cfg *Config) bool {
	return core.IsNvidiaBrevWorkRootExplicit(cfg)
}

func markNvidiaBrevWorkRootExplicit(cfg *Config) {
	core.MarkNvidiaBrevWorkRootExplicit(cfg)
}

func markWorkRootExplicit(cfg *Config) {
	core.MarkWorkRootExplicit(cfg)
}

func effectiveNvidiaBrevWorkRoot(cfg Config) string {
	return core.EffectiveNvidiaBrevWorkRoot(cfg)
}

func cliDoctorResult(provider string, leases int, runtime string) DoctorResult {
	return core.CLIDoctorResult(provider, leases, runtime)
}

var newLeaseID = core.NewLeaseID

func directLeaseLabels(cfg Config, leaseID, slug, provider, market string, keep bool) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, provider, market, keep, time.Now().UTC())
}

func touchDirectLeaseLabels(labels map[string]string, cfg Config, state string) map[string]string {
	return core.TouchDirectLeaseLabels(labels, cfg, state, time.Now().UTC())
}

func claimLeaseTargetForRepoConfig(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, reclaim bool) error {
	return core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, target, repoRoot, cfg.IdleTimeout, reclaim)
}

func claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, reclaim bool, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
	return core.ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, server, target, repoRoot, cfg.IdleTimeout, reclaim, expected, expectedExists)
}

func updateLeaseClaimEndpointIfUnchangedAfter(leaseID string, expected LeaseClaim, server Server, target SSHTarget, action func() error) (LeaseClaim, error) {
	return core.UpdateLeaseClaimEndpointIfUnchangedAfter(leaseID, expected, server, target, action)
}

func updateLeaseClaimEndpointIfUnchanged(leaseID string, expected LeaseClaim, server Server, target SSHTarget) (LeaseClaim, error) {
	return core.UpdateLeaseClaimEndpointIfUnchanged(leaseID, expected, server, target)
}

func resolveLeaseClaimForProvider(identifier string) (LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProvider(identifier, providerName)
}

func resolveLeaseClaimForProviderCloudID(cloudID string) (LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProviderCloudID(cloudID, providerName)
}

func listLeaseClaims() ([]LeaseClaim, error) {
	return core.ListLeaseClaims()
}

func removeLeaseClaimIfUnchangedAfter(leaseID string, expected LeaseClaim, action func() error) error {
	return core.RemoveLeaseClaimIfUnchangedAfter(leaseID, expected, action)
}

var waitForSSH = core.WaitForSSH

func isNvidiaBrevProviderName(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerName, "brev", "nvidia":
		return true
	default:
		return false
	}
}

func isDefaultWorkRoot(value string) bool {
	return core.IsDefaultWorkRoot(value)
}
