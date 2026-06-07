package cli

import (
	"fmt"
	"time"
)

type LeaseClaim = leaseClaim

func BaseConfig() Config {
	return baseConfig()
}

func NormalizeTargetConfig(cfg *Config) {
	normalizeTargetConfig(cfg)
}

func ExpandUserPath(path string) string {
	return expandUserPath(path)
}

func ApplyLeaseDuration(target *time.Duration, value string) error {
	if value == "" {
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fmt.Errorf("invalid duration %q", value)
	}
	*target = parsed
	return nil
}

func ServerTypeForProviderClass(provider, class string) string {
	return serverTypeForProviderClass(provider, class)
}

func AWSInstanceTypeCandidatesForConfig(cfg Config) []string {
	return awsInstanceTypeCandidatesForConfig(cfg)
}

func AWSInstanceTypeCandidatesForClass(class string) []string {
	return awsInstanceTypeCandidatesForClass(class)
}

func AzureVMSizeCandidatesForConfig(cfg Config) []string {
	return azureVMSizeCandidatesForConfig(cfg)
}

func AzureVMSizeCandidatesForClass(class string) []string {
	return azureVMSizeCandidatesForClass(class)
}

func GCPMachineTypeCandidatesForClass(class string) []string {
	return gcpMachineTypeCandidatesForClass(class)
}

func ProxmoxServerTypeForConfig(cfg Config) string {
	return proxmoxServerTypeForConfig(cfg)
}

func Exit(code int, format string, args ...any) ExitError {
	return exit(code, format, args...)
}

func ClaimLeaseForRepoProvider(leaseID, slug, provider, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProvider(leaseID, slug, provider, repoRoot, idleTimeout, reclaim)
}

func ClaimLeaseForRepoProviderScope(leaseID, slug, provider, providerScope, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderScope(leaseID, slug, provider, providerScope, repoRoot, idleTimeout, reclaim)
}

func ClaimLeaseForRepoProviderWithPond(leaseID, slug, provider, pond, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderWithPond(leaseID, slug, provider, pond, repoRoot, idleTimeout, reclaim)
}

// ClaimLeaseForRepoProviderPond is the pond-aware variant exposed for
// delegated providers that need to persist the pond label in the local claim
// sidecar (delegated providers do not own a provider-side label store).
func ClaimLeaseForRepoProviderPond(leaseID, slug, provider, pond, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderScopePond(leaseID, slug, provider, "", pond, repoRoot, idleTimeout, reclaim)
}

// ClaimLeaseForRepoProviderScopePond combines a provider scope (e.g. Docker
// context for local-container claim isolation) with the pond label so both
// features coexist in the same claim sidecar without one overwriting the other.
func ClaimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim)
}

func ResolveLeaseClaim(identifier string) (LeaseClaim, bool, error) {
	return resolveLeaseClaim(identifier)
}

func ResolveLeaseClaimForProvider(identifier, provider string) (LeaseClaim, bool, error) {
	return resolveLeaseClaimForProvider(identifier, provider)
}

func RemoveLeaseClaim(leaseID string) {
	removeLeaseClaim(leaseID)
}

func UpdateLeaseClaimCacheVolumes(leaseID string, specs []string) error {
	return updateLeaseClaimCacheVolumes(leaseID, specs)
}

func UpdateLeaseClaimEndpoint(leaseID string, server Server, target SSHTarget) error {
	return updateLeaseClaimEndpoint(leaseID, server, target)
}

func ListLeaseClaims() ([]LeaseClaim, error) {
	return listLeaseClaims()
}

func ReadLeaseClaim(leaseID string) (LeaseClaim, error) {
	return readLeaseClaim(leaseID)
}

func DirectLeaseLabels(cfg Config, leaseID, slug, provider, market string, keep bool, now time.Time) map[string]string {
	return directLeaseLabels(cfg, leaseID, slug, provider, market, keep, now)
}

func TouchDirectLeaseLabels(labels map[string]string, cfg Config, state string, now time.Time) map[string]string {
	return touchDirectLeaseLabels(labels, cfg, state, now)
}

func LeaseLabelTime(t time.Time) string {
	return leaseLabelTime(t)
}

func LeaseLabelTimeDisplay(value string) string {
	return leaseLabelTimeDisplay(value)
}

func LeaseLabelDurationDisplay(secondsValue, fallbackValue string) string {
	return leaseLabelDurationDisplay(secondsValue, fallbackValue)
}

func NewLeaseSlug(leaseID string) string {
	return newLeaseSlug(leaseID)
}

func NormalizeLeaseSlug(value string) string {
	return normalizeLeaseSlug(value)
}

func LeaseProviderName(leaseID, slug string) string {
	return leaseProviderName(leaseID, slug)
}

func AllocateDirectLeaseSlug(leaseID, requested string, servers []Server) (string, error) {
	return allocateDirectLeaseSlug(leaseID, requested, servers)
}

func AllocateClaimLeaseSlug(leaseID, requested string) (string, error) {
	return allocateClaimLeaseSlug(leaseID, requested)
}

func ServerSlug(server Server) string {
	return serverSlug(server)
}

func ServerProviderKey(server Server) string {
	return serverProviderKey(server)
}

func SetGCPProjectExplicit(cfg *Config, project string) {
	cfg.GCPProject = project
	cfg.gcpProjectExplicit = true
}

func ApplyParallelsHostRefConfig(cfg *Config, hostRef string) {
	applyParallelsHostRefConfig(cfg, hostRef)
}

func IsCanonicalLeaseID(value string) bool {
	return isCanonicalLeaseID(value)
}

func PowershellCommand(script string) string {
	return powershellCommand(script)
}

func ValidCrabboxProviderKey(value string) bool {
	return validCrabboxProviderKey(value)
}

const (
	CheckpointKindAWSAMI           = checkpointKindAWSAMI
	CheckpointKindAWSEBS           = checkpointKindAWSEBS
	CheckpointKindAzure            = checkpointKindAzure
	CheckpointKindAzureOS          = checkpointKindAzureOS
	CheckpointKindGCP              = checkpointKindGCP
	CheckpointKindGCPDisk          = checkpointKindGCPDisk
	CheckpointKindParallels        = checkpointKindParallels
	CheckpointStrategyImage        = checkpointStrategyImage
	CheckpointStrategyDiskSnapshot = checkpointStrategyDiskSnapshot
)

func NormalizeCheckpointStrategy(value string) string {
	return normalizeCheckpointStrategy(value)
}
