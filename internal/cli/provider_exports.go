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

func LoadConfig() (Config, error) {
	return loadConfig()
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

func ClaimLeaseForRepoProviderScopePondIfUnchanged(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
	return claimLeaseForRepoProviderScopePondIfUnchanged(leaseID, slug, provider, providerScope, pond, repoRoot, idleTimeout, reclaim, expected, expectedExists)
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

// ClaimLeaseTargetForConfig records a provider resource that is not yet
// attached to a repository.
func ClaimLeaseTargetForConfig(leaseID, slug string, cfg Config, server Server, target SSHTarget, idleTimeout time.Duration) error {
	return claimLeaseTargetForConfig(leaseID, slug, cfg, server, target, idleTimeout)
}

func ClaimLeaseTargetForConfigIfUnchanged(leaseID, slug string, cfg Config, server Server, target SSHTarget, idleTimeout time.Duration, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
	return claimLeaseTargetForConfigIfUnchanged(leaseID, slug, cfg, server, target, idleTimeout, expected, expectedExists)
}

// ClaimLeaseTargetForConfigScopeIfUnchanged lets a provider bind a claim to
// routing identity that is intentionally not part of the shared Config model.
func ClaimLeaseTargetForConfigScopeIfUnchanged(leaseID, slug string, cfg Config, providerScope string, server Server, target SSHTarget, idleTimeout time.Duration, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
	return claimLeaseTargetForConfigScopeIfUnchanged(leaseID, slug, cfg, providerScope, server, target, idleTimeout, expected, expectedExists)
}

func ClaimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug string, cfg Config, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
	return claimLeaseTargetForRepoConfigIfUnchanged(leaseID, slug, cfg, server, target, repoRoot, idleTimeout, reclaim, expected, expectedExists)
}

// ClaimLeaseTargetForRepoConfigScopeIfUnchanged lets a provider bind a
// repository-scoped claim to routing identity outside the shared Config model.
func ClaimLeaseTargetForRepoConfigScopeIfUnchanged(leaseID, slug string, cfg Config, providerScope string, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
	return claimLeaseTargetForRepoConfigScopeIfUnchanged(leaseID, slug, cfg, providerScope, server, target, repoRoot, idleTimeout, reclaim, expected, expectedExists)
}

// ClaimLeaseTargetForRepoConfigScopeReplacingEndpointIfUnchanged binds an
// exact resource while atomically replacing any previously published route.
func ClaimLeaseTargetForRepoConfigScopeReplacingEndpointIfUnchanged(leaseID, slug string, cfg Config, providerScope string, server Server, target SSHTarget, repoRoot string, idleTimeout time.Duration, reclaim bool, expected LeaseClaim, expectedExists bool) (LeaseClaim, error) {
	return claimLeaseTargetForRepoConfigScopeReplacingEndpointIfUnchanged(leaseID, slug, cfg, providerScope, server, target, repoRoot, idleTimeout, reclaim, expected, expectedExists)
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

func ProviderClaimScope(provider string, cfg Config) string {
	return providerClaimScope(canonicalClaimProvider(provider), cfg)
}

func RemoveLeaseClaim(leaseID string) {
	removeLeaseClaim(leaseID)
}

func RemoveLeaseClaimIfUnchanged(leaseID string, expected LeaseClaim) error {
	return removeLeaseClaimIfUnchanged(leaseID, expected)
}

func VerifyLeaseClaimUnchanged(leaseID string, expected LeaseClaim) error {
	return verifyLeaseClaimUnchanged(leaseID, expected)
}

// RemoveLeaseClaimIfUnchangedAfter holds the claim lock across action and
// removes the claim only when it still matches expected.
func RemoveLeaseClaimIfUnchangedAfter(leaseID string, expected LeaseClaim, action func() error) error {
	return removeLeaseClaimIfUnchangedAfter(leaseID, expected, action)
}

// CleanupLeaseClaimIfUnchangedAfter holds the claim lock across action and
// cleans up only when claim presence and content still match the expectation.
func CleanupLeaseClaimIfUnchangedAfter(leaseID string, expected LeaseClaim, expectedExists bool, action func() error) error {
	return cleanupLeaseClaimIfUnchangedAfter(leaseID, expected, expectedExists, action)
}
func RestoreLeaseClaimIfUnchanged(leaseID string, current, previous LeaseClaim, previousExists bool) error {
	return restoreLeaseClaimIfUnchanged(leaseID, current, previous, previousExists)
}

func ReplaceLeaseClaimIfUnchanged(leaseID string, current, replacement LeaseClaim) error {
	return replaceLeaseClaimIfUnchanged(leaseID, current, replacement)
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

func UpdateLeaseClaimEndpointIfUnchanged(leaseID string, expected LeaseClaim, server Server, target SSHTarget) (LeaseClaim, error) {
	return updateLeaseClaimEndpointIfUnchanged(leaseID, expected, server, target)
}

func UpdateLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID string, expected LeaseClaim, server Server, target SSHTarget) (LeaseClaim, error) {
	return updateLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID, expected, server, target)
}

func ReplaceLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID string, expected LeaseClaim, server Server, target SSHTarget) (LeaseClaim, error) {
	return replaceLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID, expected, server, target)
}

// UpdateLeaseClaimEndpointIfUnchangedAfter holds the claim lock while action
// runs, then updates the endpoint only if the claim still matches expected.
func UpdateLeaseClaimEndpointIfUnchangedAfter(leaseID string, expected LeaseClaim, server Server, target SSHTarget, action func() error) (LeaseClaim, error) {
	return updateLeaseClaimEndpointIfUnchangedAfter(leaseID, expected, server, target, action)
}

func UpdateLeaseClaimLabelsIfUnchanged(leaseID string, expected LeaseClaim, labels map[string]string) (LeaseClaim, error) {
	return updateLeaseClaimLabelsIfUnchanged(leaseID, expected, labels)
}

func UpdateLeaseClaimLabelsIfUnchangedAfter(leaseID string, expected LeaseClaim, labels map[string]string, action func() error) (LeaseClaim, error) {
	return updateLeaseClaimLabelsIfUnchangedAfter(leaseID, expected, labels, action)
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

// SetServerLeaseClaimSnapshot carries the exact claim state that authorized a
// provider result into a later lifecycle operation.
func SetServerLeaseClaimSnapshot(server *Server, claim LeaseClaim, exists bool) {
	if server == nil {
		return
	}
	server.claimSnapshotSet = true
	server.claimSnapshotExists = exists
	server.claimSnapshot = leaseClaim{}
	if exists {
		server.claimSnapshot = cloneLeaseClaim(claim)
	}
}

// ServerLeaseClaimSnapshot returns the carried claim, whether it existed, and
// whether a snapshot was explicitly attached.
func ServerLeaseClaimSnapshot(server Server) (LeaseClaim, bool, bool) {
	if !server.claimSnapshotSet {
		return LeaseClaim{}, false, false
	}
	return cloneLeaseClaim(server.claimSnapshot), server.claimSnapshotExists, true
}

func OSImageWasExplicit(cfg Config) bool {
	return cfg.osImageExplicit
}

func ClassWasExplicit(cfg Config) bool {
	return cfg.classExplicitOrder != 0
}

func MarkClassExplicit(cfg *Config) {
	cfg.explicitSelectionOrder++
	cfg.classExplicitOrder = cfg.explicitSelectionOrder
}

func PhalaInstanceTypeWasExplicit(cfg Config) bool {
	return cfg.phalaTypeExplicitOrder != 0
}

func MarkPhalaInstanceTypeExplicit(cfg *Config) {
	cfg.explicitSelectionOrder++
	cfg.phalaTypeExplicitOrder = cfg.explicitSelectionOrder
}

func PhalaInstanceTypeOverridesClass(cfg Config) bool {
	return cfg.phalaTypeExplicitOrder > cfg.classExplicitOrder
}

func SetOSImageExplicit(cfg *Config) {
	cfg.osImageExplicit = true
}

func OVHImageWasExplicit(cfg Config) bool {
	return cfg.ovhImageExplicit
}

func SetOVHImageExplicit(cfg *Config) {
	cfg.ovhImageExplicit = true
}

func ScalewayRegionWasExplicit(cfg Config) bool {
	return cfg.scalewayRegionExplicit
}

func SetScalewayRegionExplicit(cfg *Config) {
	cfg.scalewayRegionExplicit = true
}

func ScalewayZoneWasExplicit(cfg Config) bool {
	return cfg.scalewayZoneExplicit
}

func SetScalewayZoneExplicit(cfg *Config) {
	cfg.scalewayZoneExplicit = true
}

func ScalewayImageWasExplicit(cfg Config) bool {
	return cfg.scalewayImageExplicit
}

func SetScalewayImageExplicit(cfg *Config) {
	cfg.scalewayImageExplicit = true
}

func ScalewayTypeWasExplicit(cfg Config) bool {
	return cfg.scalewayTypeExplicit
}

func SetScalewayTypeExplicit(cfg *Config) {
	cfg.scalewayTypeExplicit = true
}

func TencentCloudRegionWasExplicit(cfg Config) bool {
	return cfg.tencentCloudRegionExplicit
}

func SetTencentCloudRegionExplicit(cfg *Config) {
	cfg.tencentCloudRegionExplicit = true
}

func TencentCloudZoneWasExplicit(cfg Config) bool {
	return cfg.tencentCloudZoneExplicit
}

func SetTencentCloudZoneExplicit(cfg *Config) {
	cfg.tencentCloudZoneExplicit = true
}

func TencentCloudImageWasExplicit(cfg Config) bool {
	return cfg.tencentCloudImageExplicit
}

func SetTencentCloudImageExplicit(cfg *Config) {
	cfg.tencentCloudImageExplicit = true
}

func TencentCloudTypeWasExplicit(cfg Config) bool {
	return cfg.tencentCloudTypeExplicit
}

func SetTencentCloudTypeExplicit(cfg *Config) {
	cfg.tencentCloudTypeExplicit = true
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

func LocalProcessStartIdentity(pid int) (string, error) {
	return webVNCDaemonProcessStartIdentity(pid)
}

func LocalProcessBootIdentity() (string, error) {
	return processBootIdentity()
}

func LocalProcessBootIdentityRequired() bool {
	return processBootIdentityRequired()
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

func WindowsBootstrapPowerShell(cfg Config, publicKey string) string {
	return windowsBootstrapPowerShell(cfg, publicKey)
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
