package cli

import "time"

type LeaseClaim = leaseClaim

func BaseConfig() Config {
	return baseConfig()
}

func ServerTypeForProviderClass(provider, class string) string {
	return serverTypeForProviderClass(provider, class)
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

func ClaimLeaseForRepoProviderWithCrew(leaseID, slug, provider, crew, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderWithCrew(leaseID, slug, provider, crew, repoRoot, idleTimeout, reclaim)
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

func IsCanonicalLeaseID(value string) bool {
	return isCanonicalLeaseID(value)
}
