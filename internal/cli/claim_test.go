package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaimLeaseForRepoWritesAndUpdatesClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	if err := claimLeaseForRepoProvider("cbx_123", "blue-lobster", "blacksmith-testbox", repo, 30*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim("cbx_123")
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID != "cbx_123" || claim.Slug != "blue-lobster" || claim.RepoRoot != repo || claim.IdleTimeoutSeconds != 1800 {
		t.Fatalf("unexpected claim: %#v", claim)
	}
	if claim.Provider != "blacksmith-testbox" {
		t.Fatalf("provider=%q", claim.Provider)
	}
}

func TestClaimLeaseForRepoConfigScopesStaticProviderClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	cfg := Config{Provider: "ssh"}
	if err := claimLeaseForRepoConfig("cbx_static", "mac-mini", cfg, repo, 10*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim("cbx_static")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != staticProvider {
		t.Fatalf("provider=%q want %q", claim.Provider, staticProvider)
	}

	cfg.Provider = "aws"
	if err := claimLeaseForRepoConfig("cbx_aws", "cloud-box", cfg, repo, 0, false); err != nil {
		t.Fatal(err)
	}
	claim, err = readLeaseClaim("cbx_aws")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != "" {
		t.Fatalf("provider=%q want empty for managed provider claim", claim.Provider)
	}
}

func TestClaimLeaseForRepoRejectsOtherRepoUnlessReclaimed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	firstRepo := filepath.Join(t.TempDir(), "first")
	secondRepo := filepath.Join(t.TempDir(), "second")
	if err := claimLeaseForRepo("cbx_123", "blue-lobster", firstRepo, 30*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	err := claimLeaseForRepo("cbx_123", "blue-lobster", secondRepo, 30*time.Minute, false)
	if err == nil || !strings.Contains(err.Error(), "use --reclaim") {
		t.Fatalf("expected reclaim error, got %v", err)
	}
	if err := claimLeaseForRepo("cbx_123", "blue-lobster", secondRepo, 30*time.Minute, true); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim("cbx_123")
	if err != nil {
		t.Fatal(err)
	}
	if claim.RepoRoot != secondRepo {
		t.Fatalf("repo root=%q want %q", claim.RepoRoot, secondRepo)
	}
}

func TestClaimLeaseForRepoIgnoresIncompleteClaimAndRemoveIsIdempotent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepo("", "slug", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepo("cbx_empty", "slug", "", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	path, err := leaseClaimPath("cbx_abc123abc123")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"leaseID":"cbx_abc123abc123"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepo("cbx_abc123abc123", "blue-lobster", "/repo", 0, false); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim("cbx_abc123abc123")
	if err != nil {
		t.Fatal(err)
	}
	if claim.RepoRoot != "/repo" || claim.ClaimedAt == "" || claim.LastUsedAt == "" || claim.IdleTimeoutSeconds != 0 {
		t.Fatalf("unexpected claim: %#v", claim)
	}
	removeLeaseClaim("cbx_abc123abc123")
	removeLeaseClaim("cbx_abc123abc123")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("claim should be removed, stat err=%v", err)
	}
}

func TestReadLeaseClaimRejectsInvalidJSON(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path, err := leaseClaimPath("cbx_badbadbadbad")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = readLeaseClaim("cbx_badbadbadbad")
	if err == nil || !strings.Contains(err.Error(), "parse claim") {
		t.Fatalf("expected parse claim error, got %v", err)
	}
}

func TestResolveLeaseClaimFindsSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("tbx_abc123", "Blue Lobster", "blacksmith-testbox", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaim("blue-lobster")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || claim.LeaseID != "tbx_abc123" || claim.Provider != "blacksmith-testbox" {
		t.Fatalf("unexpected claim ok=%t claim=%#v", ok, claim)
	}
}

func TestResolveLeaseClaimForProviderSkipsSlugCollision(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("tbx_abc123", "Blue Lobster", "blacksmith-testbox", "/repo-a", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseForRepoProvider("tlsbx_def456", "Blue Lobster", "tensorlake", "/repo-b", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaimForProvider("blue-lobster", "tensorlake")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || claim.LeaseID != "tlsbx_def456" || claim.Provider != "tensorlake" {
		t.Fatalf("unexpected provider-scoped claim ok=%t claim=%#v", ok, claim)
	}
}

func TestResolveLeaseClaimForProviderFallsBackToUnscopedLookup(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("tbx_abc123", "Blue Lobster", "", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaimForProvider("blue-lobster", "")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || claim.LeaseID != "tbx_abc123" {
		t.Fatalf("unexpected unscoped claim ok=%t claim=%#v", ok, claim)
	}
	if claim, ok, err := resolveLeaseClaimForProvider("blue-lobster", "runpod"); err != nil || ok || claim.LeaseID != "" {
		t.Fatalf("provider mismatch resolved ok=%t claim=%#v err=%v", ok, claim, err)
	}
}

func TestResolveLeaseClaimFallbacks(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if claim, ok, err := resolveLeaseClaim(""); err != nil || ok || claim.LeaseID != "" {
		t.Fatalf("empty identifier resolved ok=%t claim=%#v err=%v", ok, claim, err)
	}
	if claim, ok, err := resolveLeaseClaim("missing-slug"); err != nil || ok || claim.LeaseID != "" {
		t.Fatalf("missing claims dir resolved ok=%t claim=%#v err=%v", ok, claim, err)
	}

	if err := claimLeaseForRepo("cbx_abc123abc123", "Blue Lobster", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if claim, ok, err := resolveLeaseClaim("cbx_abc123abc123"); err != nil || !ok || claim.Slug != "Blue Lobster" {
		t.Fatalf("direct ID resolve ok=%t claim=%#v err=%v", ok, claim, err)
	}

	dir, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	claimsDir := filepath.Join(dir, "claims")
	if err := os.MkdirAll(filepath.Join(claimsDir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claimsDir, "note.txt"), []byte("ignore me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if claim, ok, err := resolveLeaseClaim("not-blue-lobster"); err != nil || ok || claim.LeaseID != "" {
		t.Fatalf("unmatched slug resolved ok=%t claim=%#v err=%v", ok, claim, err)
	}
}

func TestClaimStateDirFallbackAndMissingClaim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	dir, err := crabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dir, home) || filepath.Base(dir) != "state" {
		t.Fatalf("state dir=%q should live under home %q and end in state", dir, home)
	}
	claim, err := readLeaseClaim("cbx_missing")
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID != "" {
		t.Fatalf("missing claim=%#v", claim)
	}
}
