package unikraftcloud

import (
	"flag"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type Config = core.Config
type UnikraftCloudConfig = core.UnikraftCloudConfig
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
type CleanupRequest = core.CleanupRequest
type DoctorRequest = core.DoctorRequest
type DoctorResult = core.DoctorResult
type Server = core.Server
type LeaseClaim = core.LeaseClaim
type Repo = core.Repo
type ExitError = core.ExitError

const (
	providerName = "unikraft-cloud"
	leasePrefix  = "ukc_"
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

func normalizeLeaseSlug(value string) string {
	return core.NormalizeLeaseSlug(value)
}

func allocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return core.AllocateClaimLeaseSlug(leaseID, requested)
}

func newLeaseID() string {
	return leasePrefix + strings.TrimPrefix(core.NewLeaseID(), "cbx_")
}

func leaseProviderName(leaseID, slug string) string {
	return core.LeaseProviderName(leaseID, slug)
}

func directLeaseLabels(cfg Config, leaseID, slug string, keep bool, now time.Time) map[string]string {
	return core.DirectLeaseLabels(cfg, leaseID, slug, providerName, "", keep, now)
}

var claimLeaseTargetForRepoConfigScopeIfUnchangedDurable = func(leaseID, slug string, cfg Config, providerScope string, server Server, repoRoot string, idleTimeout time.Duration, reclaim bool, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
	return core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(leaseID, slug, cfg, providerScope, server, core.SSHTarget{}, repoRoot, idleTimeout, reclaim, expected, expectedExists)
}

func readLeaseClaim(leaseID string) (LeaseClaim, error) {
	return core.ReadLeaseClaim(leaseID)
}

func readLeaseClaimWithPresence(leaseID string) (LeaseClaim, bool, error) {
	return core.ReadLeaseClaimWithPresence(leaseID)
}

func listUnikraftCloudLeaseClaims() ([]LeaseClaim, error) {
	return core.ListLeaseClaimsWithPrefix(leasePrefix)
}

func removeLeaseClaimIfUnchanged(leaseID string, expected LeaseClaim) error {
	return core.RemoveLeaseClaimIfUnchanged(leaseID, expected)
}

var replaceLeaseClaimIfUnchangedDurable = core.ReplaceLeaseClaimIfUnchangedDurableReturning

func shouldCleanupServer(server Server, now time.Time) (bool, string) {
	return core.ShouldCleanupServer(server, now)
}

func writeTimingJSON(w io.Writer, report core.TimingReport) error {
	return core.WriteTimingJSON(w, report)
}

type timingReport = core.TimingReport
