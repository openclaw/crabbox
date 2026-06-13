package blaxel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

func blaxelEndpointWorkspaceScope(baseURL, workspace string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(baseURL) + "\n" + strings.TrimSpace(workspace)))
	return "endpoint-workspace-sha256:" + hex.EncodeToString(digest[:])
}

func newBlaxelClaimScope(baseURL, workspace string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", exit(5, "generate blaxel ownership token: %v", err)
	}
	return blaxelEndpointWorkspaceScope(baseURL, workspace) + "/ownership:" + hex.EncodeToString(token[:]), nil
}

func blaxelClaimMatchesEndpointWorkspace(claim LeaseClaim, baseURL, workspace string) bool {
	return strings.HasPrefix(strings.TrimSpace(claim.ProviderScope), blaxelEndpointWorkspaceScope(baseURL, workspace)+"/ownership:")
}

func validateBlaxelClaimScope(claim LeaseClaim, baseURL, workspace string) error {
	if !blaxelClaimMatchesEndpointWorkspace(claim, baseURL, workspace) {
		return exit(4, "blaxel lease %q belongs to a different API endpoint or workspace; restore the settings used to create it", claim.LeaseID)
	}
	return nil
}

func blaxelLeaseID(sandboxID string) string {
	return leasePrefix + strings.TrimSpace(sandboxID)
}

func blaxelSandboxID(leaseID string) string {
	return strings.TrimPrefix(strings.TrimSpace(leaseID), leasePrefix)
}

func blaxelLabels(leaseID, slug, claimScope string, repo Repo) map[string]string {
	labels := map[string]string{
		"crabbox":          "true",
		"crabbox.provider": providerName,
		"crabbox.lease":    leaseID,
		"crabbox.slug":     slug,
		blaxelClaimKey:     claimScope,
	}
	if repoSlug := repoLabel(repo); repoSlug != "" {
		labels["crabbox.repo"] = repoSlug
	}
	return labels
}

func repoLabel(repo Repo) string {
	if slug := normalizeLeaseSlug(repo.Name); slug != "" {
		return slug
	}
	if strings.TrimSpace(repo.Head) != "" {
		sum := sha256.Sum256([]byte(repo.Head))
		return hex.EncodeToString(sum[:])[:12]
	}
	if strings.TrimSpace(repo.Root) != "" {
		sum := sha256.Sum256([]byte(repo.Root))
		return hex.EncodeToString(sum[:])[:12]
	}
	return ""
}

func newSandboxName(repo Repo) string {
	base := normalizeLeaseSlug(repo.Name)
	if base == "" {
		base = "crabbox"
	}
	base = strings.TrimPrefix(base, strings.TrimSuffix(namePrefix, "-")+"-")
	const suffixLen = 6
	maxBase := sandboxNameMaxLen - len(namePrefix) - 1 - suffixLen
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-")
	}
	if base == "" {
		base = "crabbox"
	}
	return namePrefix + base + "-" + randomSuffix()
}

func resolveLeaseID(identifier, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL, workspace string) (string, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", "", "", exit(2, "provider=blaxel requires a Crabbox-created sandbox slug or lease id")
	}
	exactLeaseID := identifier
	if !strings.HasPrefix(exactLeaseID, leasePrefix) && !strings.HasPrefix(exactLeaseID, recoveryPrefix) {
		exactLeaseID = leasePrefix + exactLeaseID
	}
	if claim, err := readLeaseClaim(exactLeaseID); err != nil {
		return "", "", "", err
	} else if claim.LeaseID == exactLeaseID && claim.Provider == providerName {
		return finishResolvedLease(claim, repoRoot, reclaim, idleTimeout, baseURL, workspace)
	}
	claim, ok, err := resolveBlaxelLeaseClaim(identifier, baseURL, workspace)
	if err != nil {
		return "", "", "", err
	}
	if ok {
		return finishResolvedLease(claim, repoRoot, reclaim, idleTimeout, baseURL, workspace)
	}
	return "", "", "", exit(4, "blaxel sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", identifier, leasePrefix)
}

func resolveBlaxelLeaseClaim(identifier, baseURL, workspace string) (LeaseClaim, bool, error) {
	claims, err := listBlaxelLeaseClaims()
	if err != nil {
		return LeaseClaim{}, false, err
	}
	for _, claim := range claims {
		if claim.Provider == providerName && claim.LeaseID == identifier {
			if err := validateBlaxelClaimScope(claim, baseURL, workspace); err != nil {
				return LeaseClaim{}, false, err
			}
			return claim, true, nil
		}
	}
	slug := normalizeLeaseSlug(identifier)
	if slug != "" {
		for _, claim := range claims {
			if claim.Provider == providerName && normalizeLeaseSlug(claim.Slug) == slug {
				if err := validateBlaxelClaimScope(claim, baseURL, workspace); err != nil {
					return LeaseClaim{}, false, err
				}
				return claim, true, nil
			}
		}
	}
	return LeaseClaim{}, false, nil
}

func finishResolvedLease(claim LeaseClaim, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL, workspace string) (string, string, string, error) {
	if err := validateBlaxelClaimScope(claim, baseURL, workspace); err != nil {
		return "", "", "", err
	}
	if repoRoot != "" {
		if err := claimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, repoRoot,
			timeoutOrDefault(idleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second), reclaim); err != nil {
			return "", "", "", err
		}
	}
	slug := claim.Slug
	if strings.TrimSpace(slug) == "" {
		slug = newLeaseSlug(claim.LeaseID)
	}
	return claim.LeaseID, blaxelSandboxID(claim.LeaseID), slug, nil
}

func verifyBlaxelClaim(ctx context.Context, client Client, leaseID, sandboxID, workspace string) (Sandbox, error) {
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		return Sandbox{}, err
	}
	if err := validateBlaxelClaimScope(claim, client.BaseURL(), workspace); err != nil {
		return Sandbox{}, err
	}
	sb, err := client.GetSandbox(ctx, sandboxID)
	if err != nil {
		return Sandbox{}, err
	}
	if err := validateBlaxelSandboxOwnership(claim, sb); err != nil {
		return Sandbox{}, err
	}
	return sb, nil
}

func validateBlaxelSandboxOwnership(claim LeaseClaim, sb Sandbox) error {
	if strings.TrimSpace(sb.ID) == "" {
		return exit(5, "blaxel sandbox for lease %q omitted its id", claim.LeaseID)
	}
	labels := sb.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	if labels["crabbox"] != "true" ||
		labels["crabbox.provider"] != providerName ||
		labels["crabbox.lease"] != claim.LeaseID ||
		labels[blaxelClaimKey] != claim.ProviderScope {
		return exit(4, "blaxel sandbox %q ownership labels do not match its local claim", sb.ID)
	}
	return nil
}

func blaxelClaimCleanupDue(claim LeaseClaim, now time.Time) (bool, string) {
	if claim.IdleTimeoutSeconds <= 0 {
		return false, "idle timeout disabled"
	}
	lastUsed, err := time.Parse(time.RFC3339, strings.TrimSpace(claim.LastUsedAt))
	if err != nil {
		return false, "invalid last-used time"
	}
	deadline := lastUsed.Add(time.Duration(claim.IdleTimeoutSeconds) * time.Second)
	if now.Before(deadline) {
		return false, "idle timeout not reached"
	}
	return true, "idle timeout"
}

func timeoutOrDefault(primary, fallback time.Duration) time.Duration {
	if primary > 0 {
		return primary
	}
	return fallback
}

func randomSuffix() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:6]
	}
	return hex.EncodeToString(b[:])
}
