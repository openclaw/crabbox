package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestLeaseClaimPathRejectsTraversalIDs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	if err := claimLeaseForRepoProvider("../target", "bad", "proxmox", repo, 30*time.Minute, false); err == nil {
		t.Fatal("claim with traversal lease id succeeded")
	}
	if err := claimLeaseForRepoProvider(" cbx_123 ", "bad", "proxmox", repo, 30*time.Minute, false); err == nil {
		t.Fatal("claim with whitespace-padded lease id succeeded")
	}
	if err := claimLeaseForRepoProvider("site:runner", "bad", "proxmox", repo, 30*time.Minute, false); err == nil {
		t.Fatal("claim with Windows-reserved lease id character succeeded")
	}
	if err := claimLeaseForRepoProvider("CON", "bad", "proxmox", repo, 30*time.Minute, false); err == nil {
		t.Fatal("claim with Windows-reserved device name succeeded")
	}
	if _, ok, err := resolveLeaseClaim("../target"); err != nil || ok {
		t.Fatalf("resolve traversal id ok=%t err=%v, want no direct claim match", ok, err)
	}
}

func TestLeaseClaimPathAllowsCustomFilenameIDs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	for _, leaseID := range []string{"host@example.com", "site-runner", "東京"} {
		if err := claimLeaseForRepoProvider(leaseID, "custom", "static", repo, 30*time.Minute, false); err != nil {
			t.Fatalf("claimLeaseForRepoProvider(%q): %v", leaseID, err)
		}
		if claim, ok, err := resolveLeaseClaim(leaseID); err != nil || !ok || claim.LeaseID != leaseID {
			t.Fatalf("resolve %q claim=%#v ok=%t err=%v", leaseID, claim, ok, err)
		}
	}
}

func TestClaimLeaseTargetForRepoConfigStoresEndpointMetadata(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.Pond = "Alpha Pond"
	cfg.Cache.Volumes = []CacheVolumeConfig{{
		Key:  "repo-linux-node24-lock",
		Path: "/var/cache/crabbox/pnpm",
	}}
	server := Server{
		Provider: "aws",
		Labels: map[string]string{
			"tailscale":      "true",
			"tailscale_ipv4": "100.64.1.10",
			"slug":           "web",
			pondLabelKey:     "alpha-pond",
		},
	}
	target := SSHTarget{Host: "203.0.113.10", Port: "2222"}

	if err := claimLeaseTargetForRepoConfig("cbx_123", "web", cfg, server, target, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim("cbx_123")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Pond != "alpha-pond" || claim.TailscaleIPv4 != "100.64.1.10" || claim.SSHHost != "203.0.113.10" || claim.SSHPort != 2222 {
		t.Fatalf("unexpected claim endpoint metadata: %#v", claim)
	}
	if len(claim.CacheVolumes) != 1 || claim.CacheVolumes[0] != "repo-linux-node24-lock:/var/cache/crabbox/pnpm" {
		t.Fatalf("cache volumes not stored in claim: %#v", claim.CacheVolumes)
	}
	if claim.Labels[pondLabelKey] != "alpha-pond" {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
}

func TestClaimLeaseForRepoProviderStoresPond(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	if err := claimLeaseForRepoProviderWithPond("isb_crabbox-test", "web", "islo", "Alpha Pond", repo, 30*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim("isb_crabbox-test")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Pond != "alpha-pond" {
		t.Fatalf("pond=%q want alpha-pond", claim.Pond)
	}
	if err := claimLeaseForRepoProvider("isb_crabbox-test", "web", "islo", repo, 30*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, err = readLeaseClaim("isb_crabbox-test")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Pond != "alpha-pond" {
		t.Fatalf("pond should be preserved when omitted, got %q", claim.Pond)
	}
}

func TestClaimLeaseForRepoProviderScopePondCacheVolumesStoresInitialClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	specs := []string{"go-build:/var/cache/crabbox/go", "npm:/var/cache/crabbox/npm"}
	if err := claimLeaseForRepoProviderScopePondCacheVolumes("cbx_cache", "cache", "local-container", "runtime:docker/context:desktop", "Alpha Pond", repo, 30*time.Minute, false, specs); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim("cbx_cache")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != "local-container" || claim.ProviderScope != "runtime:docker/context:desktop" || claim.Pond != "alpha-pond" {
		t.Fatalf("unexpected claim identity: %#v", claim)
	}
	if strings.Join(claim.CacheVolumes, "\n") != strings.Join(specs, "\n") {
		t.Fatalf("cache volumes=%#v, want %#v", claim.CacheVolumes, specs)
	}

	if err := claimLeaseForRepoProviderScopePondCacheVolumes("cbx_cache", "cache", "local-container", "runtime:docker/context:desktop", "Alpha Pond", repo, 30*time.Minute, true, []string{}); err != nil {
		t.Fatal(err)
	}
	claim, err = readLeaseClaim("cbx_cache")
	if err != nil {
		t.Fatal(err)
	}
	if len(claim.CacheVolumes) != 0 {
		t.Fatalf("cache volumes not cleared on reclaim: %#v", claim.CacheVolumes)
	}
}

func TestClaimLeaseForRepoProviderScopePondEndpointStoresInitialClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	server := Server{
		Labels: map[string]string{
			"lease":    "cbx_tart",
			"instance": "crabbox-cache-1234",
			"state":    "ready",
		},
	}
	target := SSHTarget{Host: "192.0.2.44", Port: "2222"}

	if err := claimLeaseForRepoProviderScopePondEndpoint("cbx_tart", "mac", "tart", "instance:crabbox-cache-1234", "Mac Pond", repo, 30*time.Minute, false, server, target); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim("cbx_tart")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != "tart" || claim.ProviderScope != "instance:crabbox-cache-1234" || claim.Pond != "mac-pond" {
		t.Fatalf("unexpected claim identity: %#v", claim)
	}
	if claim.SSHHost != "192.0.2.44" || claim.SSHPort != 2222 || claim.Labels["instance"] != "crabbox-cache-1234" {
		t.Fatalf("endpoint metadata not stored in initial claim: %#v", claim)
	}
}

func TestLeaseClaimConcurrentMutationsRemainAtomic(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_atomic"
	if err := claimLeaseForRepoProvider(leaseID, "atomic", "docker-sandbox", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	readErr := make(chan error, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			select {
			case <-done:
				return
			default:
				if _, err := readLeaseClaim(leaseID); err != nil {
					select {
					case readErr <- err:
					default:
					}
					return
				}
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()

	errs := make(chan error, 200)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			errs <- updateLeaseClaimEndpoint(leaseID, Server{
				Labels: map[string]string{
					"state":     "ready",
					"iteration": fmt.Sprintf("%d", i),
				},
			}, SSHTarget{Host: fmt.Sprintf("host-%03d.example", i), Port: "2202"})
		}(i)
		go func(i int) {
			defer wg.Done()
			errs <- updateLeaseClaimCacheVolumes(leaseID, []string{
				fmt.Sprintf("cache-%03d:/var/cache/crabbox", i),
				"shared:/var/cache/shared",
			})
		}(i)
	}
	wg.Wait()
	close(done)
	<-readDone
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-readErr:
		t.Fatalf("concurrent read failed: %v", err)
	default:
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != "docker-sandbox" || claim.RepoRoot != "/repo" {
		t.Fatalf("claim identity lost: %#v", claim)
	}
	if claim.SSHHost == "" || claim.SSHPort != 2202 {
		t.Fatalf("endpoint update lost: %#v", claim)
	}
	if len(claim.CacheVolumes) != 2 || !strings.Contains(claim.CacheVolumes[0], ":/var/cache/crabbox") {
		t.Fatalf("cache volume update lost: %#v", claim.CacheVolumes)
	}
}

func TestClaimLeaseForRepoConfigScopesProviderClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	cfg := Config{Provider: "ssh"}
	cfg.Static.Host = "mac-mini.local"
	cfg.Static.User = "agent"
	cfg.Static.Port = "2222"
	cfg.Static.WorkRoot = "/work/crabbox"
	cfg.TargetOS = targetMacOS
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
	if claim.StaticHost != "mac-mini.local" {
		t.Fatalf("staticHost=%q want mac-mini.local", claim.StaticHost)
	}
	if claim.StaticUser != "agent" || claim.StaticPort != "2222" || claim.StaticWorkRoot != "/work/crabbox" || claim.TargetOS != targetMacOS {
		t.Fatalf("static claim details not stored: %#v", claim)
	}

	cfg.Static.User = ""
	cfg.Static.Port = ""
	cfg.Static.WorkRoot = ""
	if err := claimLeaseForRepoConfig("cbx_static", "mac-mini", cfg, repo, 10*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claim, err = readLeaseClaim("cbx_static")
	if err != nil {
		t.Fatal(err)
	}
	if claim.StaticUser != "" || claim.StaticPort != "" || claim.StaticWorkRoot != "" {
		t.Fatalf("static claim details should be cleared on update: %#v", claim)
	}

	cfg.Provider = "aws"
	if err := claimLeaseForRepoConfig("cbx_aws", "cloud-box", cfg, repo, 0, false); err != nil {
		t.Fatal(err)
	}
	claim, err = readLeaseClaim("cbx_aws")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != "aws" {
		t.Fatalf("provider=%q want aws", claim.Provider)
	}

	cfg = Config{Provider: "gcp", GCPProject: "project-a"}
	if err := claimLeaseForRepoConfig("cbx_gcp", "gcp-box", cfg, repo, 0, false); err != nil {
		t.Fatal(err)
	}
	claim, err = readLeaseClaim("cbx_gcp")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != "gcp" || claim.ProviderScope != "project:project-a" {
		t.Fatalf("gcp claim scope=%#v", claim)
	}

	cfg = Config{Provider: "google-cloud", GCPProject: "project-a"}
	if err := claimLeaseForRepoConfig("cbx_gcp_alias", "gcp-alias-box", cfg, repo, 0, false); err != nil {
		t.Fatal(err)
	}
	claim, err = readLeaseClaim("cbx_gcp_alias")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != "gcp" || claim.ProviderScope != "project:project-a" {
		t.Fatalf("gcp alias claim scope=%#v", claim)
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
