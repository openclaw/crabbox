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
	LeaseID       string `json:"leaseID"`
	Slug          string `json:"slug,omitempty"`
	Provider      string `json:"provider,omitempty"`
	ProviderScope string `json:"providerScope,omitempty"`
	// Pond, when non-empty, records the pond label that grouped the lease at
	// claim time. Direct providers surface `pond=<name>` through the
	// provider-label index so `crabbox list --pond <name>` works; delegated
	// providers (islo, e2b, modal, ...) do not own a label store, so the local
	// claim sidecar is the source of truth for those backends and for the
	// pond bridge plane (see internal/cli/pond_bridge.go).
	Pond               string `json:"pond,omitempty"`
	RepoRoot           string `json:"repoRoot"`
	ClaimedAt          string `json:"claimedAt"`
	LastUsedAt         string `json:"lastUsedAt"`
	IdleTimeoutSeconds int    `json:"idleTimeoutSeconds,omitempty"`
	// TailscaleIPv4 / TailscaleFQDN, when populated, are the canonical
	// tailnet address for the lease. Managed-Linux providers (AWS / Azure /
	// GCP / Hetzner / Proxmox / Static SSH) write these here so the unified
	// `pond peers` view can report transport=tailnet with a real endpoint
	// without re-querying the provider. When empty, the resolver reports
	// transport=pending with a note rather than pretending the peer is
	// reachable.
	TailscaleIPv4 string `json:"tailscaleIPv4,omitempty"`
	TailscaleFQDN string `json:"tailscaleFQDN,omitempty"`
	// SSHHost / SSHPort, when populated, are the public SSH address for
	// SSH-lease providers (exe.dev, RunPod, Daytona, Sprites, Namespace,
	// Semaphore). The unified `pond peers` view reports them as
	// `ssh://<host>:<port>`; when empty, transport=pending.
	SSHHost string `json:"sshHost,omitempty"`
	SSHPort int    `json:"sshPort,omitempty"`
	// BridgeURL, when populated, is the canonical public HTTPS endpoint for
	// delegated providers that mint per-sandbox URLs (Islo, E2B, Modal,
	// Cloudflare, Railway, Tensorlake). When empty, the per-provider bridge
	// adapter is queried at `pond peers` time.
	BridgeURL string `json:"bridgeURL,omitempty"`
	// Labels carries optional free-form labels surfaced to the unified
	// `pond peers` view (for example `role=web`). The claim sidecar is the
	// source of truth for delegated providers; direct providers can mirror
	// a subset here so the view stays uniform.
	Labels map[string]string `json:"labels,omitempty"`
}

func claimLeaseForRepo(leaseID, slug, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProvider(leaseID, slug, "", repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoConfig(leaseID, slug string, cfg Config, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	provider := canonicalClaimProvider(cfg.Provider)
	return claimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerClaimScope(provider, cfg), cfg.Pond, repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProvider(leaseID, slug, provider, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderScopePond(leaseID, slug, provider, "", "", repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProviderScope(leaseID, slug, provider, providerScope, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, "", repoRoot, idleTimeout, reclaim)
}

func claimLeaseForRepoProviderWithPond(leaseID, slug, provider, pond, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
	return claimLeaseForRepoProviderScopePond(leaseID, slug, provider, "", pond, repoRoot, idleTimeout, reclaim)
}

// claimLeaseForRepoProviderScopePond is the pond-aware variant. It is used by
// delegated providers (islo and the next ones to come online for the bridge
// plane) so the pond name is durable in the local claim sidecar even when the
// provider does not own a label store of its own.
func claimLeaseForRepoProviderScopePond(leaseID, slug, provider, providerScope, pond, repoRoot string, idleTimeout time.Duration, reclaim bool) error {
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
	if providerScope != "" {
		existing.ProviderScope = providerScope
	}
	if pond = normalizePondName(pond); pond != "" {
		existing.Pond = pond
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

func canonicalClaimProvider(provider string) string {
	if resolved, err := ProviderFor(provider); err == nil {
		return resolved.Name()
	}
	return normalizeProviderName(provider)
}

func providerClaimScope(provider string, cfg Config) string {
	switch provider {
	case "gcp":
		if cfg.GCPProject != "" {
			return "project:" + cfg.GCPProject
		}
	}
	return ""
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

func listLeaseClaims() ([]leaseClaim, error) {
	dir, err := crabboxStateDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "claims"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, exit(2, "read claims directory: %v", err)
	}
	claims := make([]leaseClaim, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		leaseID := strings.TrimSuffix(entry.Name(), ".json")
		claim, err := readLeaseClaim(leaseID)
		if err != nil {
			return nil, err
		}
		if claim.LeaseID != "" {
			claims = append(claims, claim)
		}
	}
	return claims, nil
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
