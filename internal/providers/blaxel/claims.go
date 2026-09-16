package blaxel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func blaxelEndpointWorkspaceScope(baseURL, workspace string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(baseURL) + "\n" + strings.TrimSpace(workspace)))
	return "endpoint-workspace-sha256:" + hex.EncodeToString(digest[:])
}

func newBlaxelClaimScope(baseURL, workspace string) (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", core.Exit(5, "generate blaxel ownership token: %v", err)
	}
	return blaxelEndpointWorkspaceScope(baseURL, workspace) + "/ownership:" + hex.EncodeToString(token[:]), nil
}

func blaxelClaimMatchesEndpointWorkspace(claim core.LeaseClaim, baseURL, workspace string) bool {
	return strings.HasPrefix(strings.TrimSpace(claim.ProviderScope), blaxelEndpointWorkspaceScope(baseURL, workspace)+"/ownership:")
}

func validateBlaxelClaimScope(claim core.LeaseClaim, baseURL, workspace string) error {
	if !blaxelClaimMatchesEndpointWorkspace(claim, baseURL, workspace) {
		return core.Exit(4, "blaxel lease %q belongs to a different API endpoint or workspace; restore the settings used to create it", claim.LeaseID)
	}
	return nil
}

func blaxelLeaseID(sandboxID string) string {
	return leasePrefix + strings.TrimSpace(sandboxID)
}

func blaxelSandboxID(leaseID string) string {
	return strings.TrimPrefix(strings.TrimSpace(leaseID), leasePrefix)
}

func blaxelLabels(leaseID, slug, claimScope string, repo core.Repo) map[string]string {
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

func repoLabel(repo core.Repo) string {
	if slug := core.NormalizeLeaseSlug(repo.Name); slug != "" {
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

func newSandboxName(repo core.Repo) string {
	base := core.NormalizeLeaseSlug(repo.Name)
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
	return namePrefix + base + "-" + shared.RandomSuffix()
}

func resolveLeaseID(identifier, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL, workspace string) (string, string, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", "", "", core.Exit(2, "provider=blaxel requires a Crabbox-created sandbox slug or lease id")
	}
	exactLeaseID := identifier
	if !strings.HasPrefix(exactLeaseID, leasePrefix) && !strings.HasPrefix(exactLeaseID, recoveryPrefix) {
		exactLeaseID = leasePrefix + exactLeaseID
	}
	if claim, err := core.ReadLeaseClaim(exactLeaseID); err != nil {
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
	return "", "", "", core.Exit(4, "blaxel sandbox %q is not claimed by Crabbox; use a Crabbox slug or %s<sandbox-id>", identifier, leasePrefix)
}

func resolveBlaxelLeaseClaim(identifier, baseURL, workspace string) (core.LeaseClaim, bool, error) {
	claims, err := listBlaxelLeaseClaims()
	if err != nil {
		return core.LeaseClaim{}, false, err
	}
	for _, claim := range claims {
		if claim.Provider == providerName && claim.LeaseID == identifier {
			if err := validateBlaxelClaimScope(claim, baseURL, workspace); err != nil {
				return core.LeaseClaim{}, false, err
			}
			return claim, true, nil
		}
	}
	slug := core.NormalizeLeaseSlug(identifier)
	if slug != "" {
		for _, claim := range claims {
			if claim.Provider == providerName && core.NormalizeLeaseSlug(claim.Slug) == slug {
				if err := validateBlaxelClaimScope(claim, baseURL, workspace); err != nil {
					return core.LeaseClaim{}, false, err
				}
				return claim, true, nil
			}
		}
	}
	return core.LeaseClaim{}, false, nil
}

func finishResolvedLease(claim core.LeaseClaim, repoRoot string, reclaim bool, idleTimeout time.Duration, baseURL, workspace string) (string, string, string, error) {
	if err := validateBlaxelClaimScope(claim, baseURL, workspace); err != nil {
		return "", "", "", err
	}
	if repoRoot != "" {
		if err := core.ClaimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, repoRoot,
			timeoutOrDefault(idleTimeout, time.Duration(claim.IdleTimeoutSeconds)*time.Second), reclaim); err != nil {
			return "", "", "", err
		}
	}
	slug := claim.Slug
	if strings.TrimSpace(slug) == "" {
		slug = core.NewLeaseSlug(claim.LeaseID)
	}
	return claim.LeaseID, blaxelSandboxID(claim.LeaseID), slug, nil
}

func verifyBlaxelClaim(ctx context.Context, client Client, leaseID, sandboxID, workspace string) (Sandbox, error) {
	claim, err := core.ReadLeaseClaim(leaseID)
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

func validateBlaxelSandboxOwnership(claim core.LeaseClaim, sb Sandbox) error {
	if strings.TrimSpace(sb.ID) == "" {
		return core.Exit(5, "blaxel sandbox for lease %q omitted its id", claim.LeaseID)
	}
	labels := sb.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	if labels["crabbox"] != "true" ||
		labels["crabbox.provider"] != providerName ||
		labels["crabbox.lease"] != claim.LeaseID ||
		labels[blaxelClaimKey] != claim.ProviderScope {
		return core.Exit(4, "blaxel sandbox %q ownership labels do not match its local claim", sb.ID)
	}
	return nil
}

func timeoutOrDefault(primary, fallback time.Duration) time.Duration {
	if primary > 0 {
		return primary
	}
	return fallback
}
