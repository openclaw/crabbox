package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type leaseClaim struct {
	LeaseID            string `json:"leaseID"`
	Slug               string `json:"slug,omitempty"`
	Provider           string `json:"provider,omitempty"`
	Crew               string `json:"crew,omitempty"`
	RepoRoot           string `json:"repoRoot"`
	ClaimedAt          string `json:"claimedAt"`
	LastUsedAt         string `json:"lastUsedAt"`
	IdleTimeoutSeconds int    `json:"idleTimeoutSeconds,omitempty"`
}

func claimLeaseForRepo(leaseID, slug, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProvider(leaseID, slug, "", repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoConfig(leaseID, slug string, cfg Config, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	provider := ""
	if isStaticProvider(cfg.Provider) {
		provider = staticProvider
	}
	return claimLeaseForRepoProvider(leaseID, slug, provider, repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProvider(leaseID, slug, provider, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderWithCrew(leaseID, slug, provider, "", repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProviderWithCrew(leaseID, slug, provider, crew, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	if leaseID == "" || repoRoot == "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return err
	}
	existing, err := readLeaseClaim(leaseID)
	if err != nil {
		return err
	}
	if existing.LeaseID != "" && existing.RepoRoot != "" && existing.RepoRoot != repoRoot && !reclaim {
		return exit(2, "lease %s is claimed by repo %s; use --reclaim to claim it for %s", leaseID, existing.RepoRoot, repoRoot)
	}
	if existing.ClaimedAt == "" || reclaim || existing.RepoRoot != repoRoot {
		existing.ClaimedAt = now
	}
	existing.LeaseID = leaseID
	existing.Slug = slug
	if provider != "" {
		existing.Provider = provider
	}
	if crew = normalizeCrewName(crew); crew != "" {
		existing.Crew = crew
	}
	existing.RepoRoot = repoRoot
	existing.LastUsedAt = now
	if idleTimeout > 0 {
		existing.IdleTimeoutSeconds = int(idleTimeout.Seconds())
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return exit(2, "create claim directory: %v", err)
	}
	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return exit(2, "write claim %s: %v", path, err)
	}
	return nil
}

func resolveLeaseClaim(identifier string) (leaseClaim, bool, error) {
	if identifier == "" {
		return leaseClaim{}, false, nil
	}
	if claim, err := readLeaseClaim(identifier); err != nil {
		return leaseClaim{}, false, err
	} else if claim.LeaseID != "" {
		return claim, true, nil
	}
	dir, err := crabboxStateDir()
	if err != nil {
		return leaseClaim{}, false, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "claims"))
	if errors.Is(err, os.ErrNotExist) {
		return leaseClaim{}, false, nil
	}
	if err != nil {
		return leaseClaim{}, false, exit(2, "read claims directory: %v", err)
	}
	slug := normalizeLeaseSlug(identifier)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		leaseID := strings.TrimSuffix(entry.Name(), ".json")
		claim, err := readLeaseClaim(leaseID)
		if err != nil {
			return leaseClaim{}, false, err
		}
		if claim.LeaseID == identifier || (slug != "" && normalizeLeaseSlug(claim.Slug) == slug) {
			return claim, true, nil
		}
	}
	return leaseClaim{}, false, nil
}

func resolveLeaseClaimForProvider(identifier, provider string) (leaseClaim, bool, error) {
	if provider == "" {
		return resolveLeaseClaim(identifier)
	}
	claim, ok, err := resolveLeaseClaim(identifier)
	if err != nil || !ok {
		return claim, ok, err
	}
	if claim.Provider == provider {
		return claim, true, nil
	}
	claim, ok, err = findLeaseClaim(identifier, func(candidate leaseClaim) bool {
		return candidate.Provider == provider
	})
	if err != nil || !ok {
		return leaseClaim{}, false, err
	}
	return claim, true, nil
}

func findLeaseClaim(identifier string, match func(leaseClaim) bool) (leaseClaim, bool, error) {
	if identifier == "" {
		return leaseClaim{}, false, nil
	}
	dir, err := crabboxStateDir()
	if err != nil {
		return leaseClaim{}, false, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "claims"))
	if errors.Is(err, os.ErrNotExist) {
		return leaseClaim{}, false, nil
	}
	if err != nil {
		return leaseClaim{}, false, exit(2, "read claims directory: %v", err)
	}
	slug := normalizeLeaseSlug(identifier)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		leaseID := strings.TrimSuffix(entry.Name(), ".json")
		claim, err := readLeaseClaim(leaseID)
		if err != nil {
			return leaseClaim{}, false, err
		}
		if (claim.LeaseID == identifier || (slug != "" && normalizeLeaseSlug(claim.Slug) == slug)) && match(claim) {
			return claim, true, nil
		}
	}
	return leaseClaim{}, false, nil
}

func removeLeaseClaim(leaseID string) {
	path, err := leaseClaimPath(leaseID)
	if err == nil {
		_ = os.Remove(path)
	}
}

func readLeaseClaim(leaseID string) (leaseClaim, error) {
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		return leaseClaim{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return leaseClaim{}, nil
	}
	if err != nil {
		return leaseClaim{}, exit(2, "read claim %s: %v", path, err)
	}
	var claim leaseClaim
	if err := json.Unmarshal(data, &claim); err != nil {
		return leaseClaim{}, exit(2, "parse claim %s: %v", path, err)
	}
	return claim, nil
}

func leaseClaimPath(leaseID string) (string, error) {
	dir, err := crabboxStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "claims", leaseID+".json"), nil
}

func crabboxStateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "crabbox"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", exit(2, "user state directory is unavailable")
	}
	return filepath.Join(dir, "crabbox", "state"), nil
}
