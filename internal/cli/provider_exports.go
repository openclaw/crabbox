package cli

import (
	"context"
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

func IncusServerTypeForConfig(cfg Config) string {
	return incusServerTypeForConfig(cfg)
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

func AppendDirectPondTailscaleTag(cfg *Config) {
	appendPondTailscaleTag(cfg, true)
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

func ClaimLeaseForRepoProviderScopePondCacheVolumes(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, cacheVolumes []string) error {
	return claimLeaseForRepoProviderScopePondCacheVolumes(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim, cacheVolumes)
}

func ClaimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, server Server, target SSHTarget) error {
	return claimLeaseForRepoProviderScopePondEndpoint(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim, server, target)
}

func ClaimLeaseTargetForRepoConfig(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, target, repoRoot, idleTimeout, reclaim)
}

func ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
	return claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, server, target, repoRoot, idleTimeout, reclaim, expected, expectedExists)
}

func ResolveLeaseClaim(identifier string) (LeaseClaim, bool, error) {
	return resolveLeaseClaim(identifier)
}

func ResolveLeaseClaimForProvider(identifier, provider string) (LeaseClaim, bool, error) {
	return resolveLeaseClaimForProvider(identifier, provider)
}

func ResolveLeaseClaimForProviderWithExact(identifier, provider string) (LeaseClaim, bool, bool, error) {
	return resolveLeaseClaimForProviderWithExact(identifier, provider)
}

func ResolveLeaseClaimForProviderCloudID(cloudID, provider string) (LeaseClaim, bool, error) {
	return resolveLeaseClaimForProviderCloudID(cloudID, provider)
}

func LeaseClaimMatchesIdentifier(claim LeaseClaim, identifier string) bool {
	return leaseClaimMatchesIdentifier(claim, identifier)
}

func RemoveLeaseClaim(leaseID string) {
	removeLeaseClaim(leaseID)
}

func RemoveLeaseClaimIfUnchanged(leaseID string, expected LeaseClaim) error {
	return removeLeaseClaimIfUnchanged(leaseID, expected)
}

func ValidateAzureSSHCIDRsForAcquire(ctx context.Context, cfg Config) error {
	_, err := azureSSHCIDRsForRules(ctx, cfg, nil)
	return err
}

func UpdateLeaseClaimCacheVolumes(leaseID string, specs []string) error {
	return updateLeaseClaimCacheVolumes(leaseID, specs)
}

func UpdateLeaseClaimEndpoint(leaseID string, server Server, target SSHTarget) error {
	return updateLeaseClaimEndpoint(leaseID, server, target)
}

func UpdateLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID string, expected LeaseClaim, server Server, target SSHTarget) (LeaseClaim, error) {
	return updateLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID, expected, server, target)
}

func UpdateLeaseClaimLabelsIfUnchanged(leaseID string, expected LeaseClaim, labels map[string]string) (LeaseClaim, error) {
	return updateLeaseClaimLabelsIfUnchanged(leaseID, expected, labels)
}

// UpdateLeaseClaimTailscale records a tailnet endpoint (IPv4 and/or FQDN) on an
// existing claim. Used by delegated-run providers that join the tailnet
// out-of-band rather than through a Crabbox-managed SSH lease.
func UpdateLeaseClaimTailscale(leaseID, ipv4, fqdn string) error {
	return updateLeaseClaimTailscale(leaseID, ipv4, fqdn)
}

func UpdateLeaseClaimTailscaleSettings(leaseID, hostname string, tags []string, loginURL, exitNode string, exitLAN bool) error {
	return updateLeaseClaimTailscaleSettings(leaseID, hostname, tags, loginURL, exitNode, exitLAN)
}

func ClearLeaseClaimTailscale(leaseID string) error {
	return clearLeaseClaimTailscale(leaseID)
}

func ListLeaseClaims() ([]LeaseClaim, error) {
	return listLeaseClaims()
}

func ListLeaseClaimsWithPrefix(prefix string) ([]LeaseClaim, error) {
	return listLeaseClaimsWithPrefix(prefix)
}

func ReadLeaseClaim(leaseID string) (LeaseClaim, error) {
	return readLeaseClaim(leaseID)
}

func ReadLeaseClaimWithPresence(leaseID string) (LeaseClaim, bool, error) {
	return readLeaseClaimWithPresence(leaseID)
}

func OSImageWasExplicit(cfg Config) bool {
	return cfg.osImageExplicit
}

func CrabboxStateDir() (string, error) {
	return crabboxStateDir()
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

func SlugWithCollisionSuffix(base, seed string) string {
	return slugWithCollisionSuffix(base, seed)
}

func NormalizeLeaseSlug(value string) string {
	return normalizeLeaseSlug(value)
}

func RenderTailscaleHostname(template, leaseID, slug, provider string) string {
	return renderTailscaleHostname(template, leaseID, slug, provider)
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

func ProbeSSHReady(ctx context.Context, target *SSHTarget, timeout time.Duration) bool {
	return probeSSHReady(ctx, target, timeout)
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
	CheckpointKindDockerCommit     = checkpointKindDockerCommit
	CheckpointStrategyImage        = checkpointStrategyImage
	CheckpointStrategyDiskSnapshot = checkpointStrategyDiskSnapshot
)

func NormalizeCheckpointStrategy(value string) string {
	return normalizeCheckpointStrategy(value)
}
