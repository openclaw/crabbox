package railway

import (
	"flag"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type RailwayConfig = core.RailwayConfig
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
type LeaseClaim = core.LeaseClaim

const (
	providerName = "railway"
	targetLinux  = core.TargetLinux

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

func inventoryDoctorResult(provider string, leases int) DoctorResult {
	return core.InventoryDoctorResult(provider, leases)
}

func claimLeaseTargetForConfigIfUnchanged(leaseID string, cfg Config, server Server, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
	return core.ClaimLeaseTargetForConfigIfUnchanged(leaseID, "", cfg, server, core.SSHTarget{}, 0, expected, expectedExists)
}

func resolveLeaseClaim(identifier string) (LeaseClaim, bool, error) {
	return core.ResolveLeaseClaim(identifier)
}

func resolveLeaseClaimForProviderCloudID(cloudID string) (LeaseClaim, bool, error) {
	return core.ResolveLeaseClaimForProviderCloudID(cloudID, providerName)
}

func providerClaimScope(cfg Config) string {
	return core.ProviderClaimScope(providerName, cfg)
}

func updateLeaseClaimLabelsIfUnchangedAfter(leaseID string, expected LeaseClaim, labels map[string]string, action func() error) (LeaseClaim, error) {
	return core.UpdateLeaseClaimLabelsIfUnchangedAfter(leaseID, expected, labels, action)
}

func removeLeaseClaimIfUnchanged(leaseID string, expected LeaseClaim) error {
	return core.RemoveLeaseClaimIfUnchanged(leaseID, expected)
}
