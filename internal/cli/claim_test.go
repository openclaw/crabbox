package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfirmedAbsentClaimRemovalRequiresDirectorySyncAndRetriesAfterDeletion(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_123456789abc"
	if err := claimLeaseForRepoProvider(leaseID, "fast-coral", "external", "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	expected, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	syncErr := errors.New("claim directory sync unavailable")
	err = removeLeaseClaimIfUnchangedAfterWithSync(leaseID, expected, nil, func(string) error { return syncErr })
	if err == nil || !strings.Contains(err.Error(), syncErr.Error()) {
		t.Fatalf("claim removal error=%v", err)
	}
	if _, exists, readErr := readLeaseClaimWithPresence(leaseID); readErr != nil || exists {
		t.Fatalf("removed claim exists=%t err=%v", exists, readErr)
	}
	var synced string
	if err := cleanupLeaseClaimIfUnchangedAfterWithSync(leaseID, leaseClaim{}, false, nil, func(dir string) error {
		synced = filepath.Clean(dir)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if synced != filepath.Clean(filepath.Dir(path)) {
		t.Fatalf("retry synced %q want %q", synced, filepath.Dir(path))
	}
}

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
		CloudID:  "i-123",
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
	if claim.Pond != "alpha-pond" || claim.CloudID != "i-123" || claim.TailscaleIPv4 != "100.64.1.10" || claim.SSHHost != "203.0.113.10" || claim.SSHPort != 2222 {
		t.Fatalf("unexpected claim endpoint metadata: %#v", claim)
	}
	if len(claim.CacheVolumes) != 1 || claim.CacheVolumes[0] != "repo-linux-node24-lock:/var/cache/crabbox/pnpm" {
		t.Fatalf("cache volumes not stored in claim: %#v", claim.CacheVolumes)
	}
	if claim.Labels[pondLabelKey] != "alpha-pond" {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
	if resolved, ok, err := resolveLeaseClaimForProviderCloudID("i-123", "aws"); err != nil || !ok || resolved.LeaseID != "cbx_123" {
		t.Fatalf("cloud id resolution claim=%#v ok=%t err=%v", resolved, ok, err)
	}
}

func TestClaimLeaseTargetForConfigStoresUnattachedProviderResource(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	server := Server{
		Provider: "aws",
		CloudID:  "i-1750645",
		Labels: map[string]string{
			"provider": "aws",
			"slug":     "warm",
		},
	}

	if err := claimLeaseTargetForConfig("cbx_hostinger123", "warm", cfg, server, SSHTarget{Host: "203.0.113.10"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim("cbx_hostinger123")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != "aws" || claim.CloudID != "i-1750645" || claim.RepoRoot != "" {
		t.Fatalf("unexpected unattached provider claim: %#v", claim)
	}

	repoRoot := t.TempDir()
	if err := claimLeaseTargetForRepoConfig("cbx_hostinger123", "warm", cfg, server, SSHTarget{Host: "203.0.113.10"}, repoRoot, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	claim, err = readLeaseClaim("cbx_hostinger123")
	if err != nil {
		t.Fatal(err)
	}
	if claim.RepoRoot != repoRoot {
		t.Fatalf("provider claim was not attached to repo: %#v", claim)
	}
}

func TestClaimLeaseTargetForConfigIfUnchangedStoresProviderScope(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "railway"
	cfg.Railway.APIURL = " https://railway.example.test/graphql/v2/ "
	cfg.Railway.ProjectID = " proj-1 "
	cfg.Railway.EnvironmentID = " env-1 "
	server := Server{
		Provider: "railway",
		CloudID:  "svc-1",
		Name:     "api",
		Labels: map[string]string{
			"railwayDeploymentId": "dep-1",
		},
	}
	leaseID := "railway_123456789abc"
	previous, previousExists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		t.Fatal(err)
	}

	claim, err := claimLeaseTargetForConfigIfUnchanged(leaseID, "", cfg, server, SSHTarget{}, 0, previous, previousExists)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != "railway" || claim.CloudID != "svc-1" || claim.RepoRoot != "" {
		t.Fatalf("unexpected provider claim: %#v", claim)
	}
	wantScope := "endpoint:https://railway.example.test/graphql/v2|project:proj-1|environment:env-1"
	if claim.ProviderScope != wantScope {
		t.Fatalf("ProviderScope=%q, want %q", claim.ProviderScope, wantScope)
	}
	if claim.Labels["railwayDeploymentId"] != "dep-1" {
		t.Fatalf("labels=%#v", claim.Labels)
	}

	if _, err := claimLeaseTargetForConfigIfUnchanged(leaseID, "", cfg, Server{Provider: "railway", CloudID: "svc-2"}, SSHTarget{}, 0, previous, previousExists); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("stale create-if-absent err=%v", err)
	}
}

func TestRailwayProviderClaimScopeRequiresCompleteRoute(t *testing.T) {
	cfg := baseConfig()
	cfg.Railway.APIURL = "https://railway.example.test/graphql/v2/"
	cfg.Railway.ProjectID = "proj-1"
	cfg.Railway.EnvironmentID = "env-1"
	want := "endpoint:https://railway.example.test/graphql/v2|project:proj-1|environment:env-1"
	if got := providerClaimScope("railway", cfg); got != want {
		t.Fatalf("providerClaimScope(railway)=%q, want %q", got, want)
	}
	cfg.Railway.EnvironmentID = ""
	if got := providerClaimScope("railway", cfg); got != "" {
		t.Fatalf("incomplete railway scope=%q, want empty", got)
	}
}

func TestConditionalClaimMutationRejectsChangedState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "digitalocean"
	leaseID := "cbx_conditional123"

	if err := claimLeaseTargetForRepoConfig(leaseID, "other", Config{Provider: "aws"}, Server{Provider: "aws", CloudID: "i-123"}, SSHTarget{}, "/other", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, err := claimLeaseTargetForRepoConfigIfUnchanged(leaseID, "digitalocean", cfg, Server{Provider: "digitalocean", CloudID: "77"}, SSHTarget{}, "/repo", time.Minute, false, leaseClaim{}, false); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("create-if-absent err=%v", err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != "aws" || claim.CloudID != "i-123" {
		t.Fatalf("claim overwritten: %#v", claim)
	}

	expected := claim
	if err := updateLeaseClaimEndpoint(leaseID, Server{Provider: "aws", CloudID: "i-456"}, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	if _, err := updateLeaseClaimEndpointIfUnchanged(leaseID, expected, Server{Provider: "digitalocean", CloudID: "77"}, SSHTarget{}); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("conditional update err=%v", err)
	}
	claim, err = readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != "aws" || claim.CloudID != "i-456" {
		t.Fatalf("changed claim overwritten: %#v", claim)
	}
}

func TestConditionalRepoClaimCanBeRestored(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_transaction123"

	previous, previousExists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := claimLeaseForRepoProviderScopePondIfUnchanged(leaseID, "transaction", "kubevirt", "cluster:test", "", "/repo-a", time.Minute, false, previous, previousExists)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.RepoRoot != "/repo-a" {
		t.Fatalf("claimed=%#v", claimed)
	}
	if err := restoreLeaseClaimIfUnchanged(leaseID, claimed, previous, previousExists); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("restored absent claim exists=%v err=%v", exists, err)
	}

	if err := claimLeaseForRepoProviderScope(leaseID, "transaction", "kubevirt", "cluster:test", "/repo-original", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	previous, previousExists, err = readLeaseClaimWithPresence(leaseID)
	if err != nil || !previousExists {
		t.Fatalf("previous=%#v exists=%v err=%v", previous, previousExists, err)
	}
	claimed, err = claimLeaseForRepoProviderScopePondIfUnchanged(leaseID, "transaction", "kubevirt", "cluster:test", "", "/repo-b", time.Minute, true, previous, previousExists)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreLeaseClaimIfUnchanged(leaseID, claimed, previous, previousExists); err != nil {
		t.Fatal(err)
	}
	restored, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, previous) {
		t.Fatalf("restored=%#v want %#v", restored, previous)
	}

	incompleteID := "cbx_incomplete123"
	path, err := leaseClaimPath(incompleteID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous, previousExists, err = readLeaseClaimWithPresence(incompleteID)
	if err != nil || !previousExists || previous.LeaseID != "" {
		t.Fatalf("incomplete previous=%#v exists=%v err=%v", previous, previousExists, err)
	}
	claimed, err = claimLeaseForRepoProviderScopePondIfUnchanged(incompleteID, "incomplete", "kubevirt", "cluster:test", "", "/repo", time.Minute, false, previous, previousExists)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreLeaseClaimIfUnchanged(incompleteID, claimed, previous, previousExists); err != nil {
		t.Fatal(err)
	}
	restored, restoredExists, err := readLeaseClaimWithPresence(incompleteID)
	if err != nil || !restoredExists || restored.LeaseID != "" {
		t.Fatalf("restored incomplete=%#v exists=%v err=%v", restored, restoredExists, err)
	}
}

func TestReplaceLeaseClaimIfUnchangedPreservesSelectedRuntimeState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_replace123456"
	running := Server{
		Provider: "aws",
		CloudID:  "i-replace",
		Labels:   map[string]string{"provider": "aws", "state": "running"},
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, "replace", Config{Provider: "aws"}, running, SSHTarget{Host: "192.0.2.10", Port: "22"}, "/repo-b", time.Minute, true); err != nil {
		t.Fatal(err)
	}
	current, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	replacement := cloneLeaseClaim(current)
	replacement.RepoRoot = "/repo-a"
	replacement.Labels["state"] = "stopped"
	replacement.SSHHost = ""
	replacement.SSHPort = 0
	if err := replaceLeaseClaimIfUnchanged(leaseID, current, replacement); err != nil {
		t.Fatal(err)
	}
	replaced, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.RepoRoot != "/repo-a" || replaced.Labels["state"] != "stopped" || replaced.SSHHost != "" {
		t.Fatalf("replaced=%#v", replaced)
	}
	if err := replaceLeaseClaimIfUnchanged(leaseID, current, replacement); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("stale replacement err=%v", err)
	}
}

func TestConditionalClaimCreateRejectsExistingEmptyFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_emptyclaim123"
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig()
	cfg.Provider = "digitalocean"
	if _, err := claimLeaseTargetForRepoConfigIfUnchanged(leaseID, "digitalocean", cfg, Server{Provider: "digitalocean", CloudID: "77"}, SSHTarget{}, "/repo", time.Minute, false, leaseClaim{}, false); err == nil || !strings.Contains(err.Error(), "claim is incomplete") {
		t.Fatalf("conditional create err=%v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{}" {
		t.Fatalf("empty claim changed: data=%q err=%v", data, err)
	}
}

func TestEndpointClaimRewriteRejectsExistingEmptyFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_emptyendpoint123"
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, "endpoint", Config{Provider: "aws"}, Server{Provider: "aws", CloudID: "i-123"}, SSHTarget{}, "/repo", time.Minute, false); err == nil || !strings.Contains(err.Error(), "claim is incomplete") {
		t.Fatalf("endpoint rewrite err=%v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{}" {
		t.Fatalf("empty claim changed: data=%q err=%v", data, err)
	}
	if err := updateLeaseClaimEndpoint(leaseID, Server{Provider: "aws", CloudID: "i-456"}, SSHTarget{}); err == nil || !strings.Contains(err.Error(), "claim is incomplete") {
		t.Fatalf("direct endpoint update err=%v", err)
	}
	if _, err := updateLeaseClaimEndpointIfUnchanged(leaseID, leaseClaim{}, Server{Provider: "aws", CloudID: "i-789"}, SSHTarget{}); err == nil || !strings.Contains(err.Error(), "claim is incomplete") {
		t.Fatalf("conditional endpoint update err=%v", err)
	}
}

func TestEndpointClaimRewriteRejectsUnknownExistingProvider(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_unknownprovider123"
	if err := claimLeaseTargetForRepoConfig(leaseID, "unknown", Config{Provider: "unknown-provider"}, Server{
		Provider: "unknown-provider",
		CloudID:  "unknown-1",
		Labels:   map[string]string{"lease": leaseID, "slug": "unknown", "provider": "unknown-provider"},
	}, SSHTarget{}, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, "unknown", Config{Provider: "aws"}, Server{
		Provider: "aws",
		CloudID:  "i-123",
		Labels:   map[string]string{"lease": leaseID, "slug": "unknown", "provider": "aws"},
	}, SSHTarget{}, "/repo", time.Minute, true); err == nil || !strings.Contains(err.Error(), "unavailable provider") {
		t.Fatalf("provider rewrite err=%v", err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != "unknown-provider" || claim.CloudID != "unknown-1" {
		t.Fatalf("unknown-provider claim rewritten: %#v", claim)
	}
}

func TestClaimMutationRejectsMisfiledClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	storedLeaseID := "cbx_storedclaim123"
	requestedLeaseID := "cbx_requestedclaim123"
	if err := claimLeaseTargetForRepoConfig(storedLeaseID, "stored", Config{Provider: "aws"}, Server{
		Provider: "aws",
		CloudID:  "i-stored",
		Labels:   map[string]string{"lease": storedLeaseID, "slug": "stored", "provider": "aws"},
	}, SSHTarget{}, "/stored", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	storedPath, err := leaseClaimPath(storedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	requestedPath, err := leaseClaimPath(requestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(storedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestedPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(storedPath); err != nil {
		t.Fatal(err)
	}

	err = claimLeaseTargetForRepoConfig(requestedLeaseID, "requested", Config{Provider: "aws"}, Server{
		Provider: "aws",
		CloudID:  "i-requested",
		Labels:   map[string]string{"lease": requestedLeaseID, "slug": "requested", "provider": "aws"},
	}, SSHTarget{}, "/requested", time.Minute, true)
	if err == nil || !strings.Contains(err.Error(), "refusing misfiled claim") {
		t.Fatalf("claim rewrite err=%v", err)
	}
	unchanged, err := os.ReadFile(requestedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != string(data) {
		t.Fatalf("misfiled claim rewritten:\n%s\nwant:\n%s", unchanged, data)
	}
}

func TestReadLeaseClaimWithPresenceDistinguishesEmptyAndMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_claimpresence123"
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	claim, exists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil || !exists || claim.LeaseID != "" {
		t.Fatalf("empty claim=%#v exists=%v err=%v", claim, exists, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	claim, exists, err = readLeaseClaimWithPresence(leaseID)
	if err != nil || exists || claim.LeaseID != "" {
		t.Fatalf("missing claim=%#v exists=%v err=%v", claim, exists, err)
	}
}

func TestConditionalClaimActionUpdateRejectsChangedState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_conditionaldelete123"
	if err := claimLeaseTargetForRepoConfig(leaseID, "first", Config{Provider: "aws"}, Server{Provider: "aws", CloudID: "i-123"}, SSHTarget{}, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	expected, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimEndpoint(leaseID, Server{Provider: "aws", CloudID: "i-456"}, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	actionCalled := false
	if _, err := updateLeaseClaimLabelsIfUnchangedAfter(leaseID, expected, map[string]string{"state": "stopped"}, func() error {
		actionCalled = true
		return nil
	}); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("conditional update err=%v", err)
	}
	if actionCalled {
		t.Fatal("conditional update ran action for changed claim")
	}
	changed, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if changed.CloudID != "i-456" {
		t.Fatalf("changed claim removed: %#v", changed)
	}
	updated, err := updateLeaseClaimLabelsIfUnchangedAfter(leaseID, changed, map[string]string{"state": "stopped"}, func() error {
		actionCalled = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !actionCalled {
		t.Fatal("conditional update did not run action for unchanged claim")
	}
	if updated.Labels["state"] != "stopped" {
		t.Fatalf("updated claim=%#v", updated)
	}
}

func TestConditionalClaimEndpointActionUpdatesAtomically(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_conditionalendpoint123"
	cfg := Config{Provider: "aws"}
	server := Server{Provider: "aws", CloudID: "i-123", Labels: map[string]string{"provider": "aws", "state": "running"}}
	if err := claimLeaseTargetForRepoConfig(leaseID, "endpoint", cfg, server, SSHTarget{Host: "203.0.113.10", Port: "22"}, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	expected, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	stopped := server
	stopped.Labels = map[string]string{"provider": "aws", "state": "stopped"}
	actionCalled := false
	updated, err := updateLeaseClaimEndpointIfUnchangedAfter(leaseID, expected, stopped, SSHTarget{}, func() error {
		actionCalled = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !actionCalled || updated.Labels["state"] != "stopped" || updated.SSHHost != "" || updated.SSHPort != 0 {
		t.Fatalf("updated=%#v actionCalled=%v", updated, actionCalled)
	}

	if err := updateLeaseClaimEndpoint(leaseID, Server{Provider: "aws", CloudID: "i-456"}, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	actionCalled = false
	if _, err := updateLeaseClaimEndpointIfUnchangedAfter(leaseID, updated, stopped, SSHTarget{}, func() error {
		actionCalled = true
		return nil
	}); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("conditional endpoint update err=%v", err)
	}
	if actionCalled {
		t.Fatal("conditional endpoint update ran action for changed claim")
	}
}

func TestConditionalClaimHelpersAndExactResolution(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_helperclaim123"
	slug := "helper-claim"
	cfg := baseConfig()
	cfg.Provider = "aws"
	server := Server{
		Provider: "aws",
		CloudID:  "i-123",
		Labels:   map[string]string{"lease": leaseID, "slug": slug, "provider": "aws"},
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, SSHTarget{}, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	expected, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	server.CloudID = "i-456"
	server.Labels["state"] = "ready"
	updated, err := updateLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID, expected, server, SSHTarget{Host: "203.0.113.10", Port: "22"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.CloudID != "i-456" || updated.SSHHost != "203.0.113.10" || updated.Labels["state"] != "ready" {
		t.Fatalf("updated claim=%#v", updated)
	}
	if err := mutateLeaseClaim(leaseID, func(claim *leaseClaim) error {
		claim.TailscaleIPv4 = "100.64.0.1"
		claim.TailscaleFQDN = "old.tail.example"
		claim.BridgeURL = "https://old.example"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	updated, err = readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	server.CloudID = "i-789"
	replaced, err := replaceLeaseClaimEndpointIfUnchangedWithProviderMetadata(leaseID, updated, server, SSHTarget{})
	if err != nil {
		t.Fatal(err)
	}
	if replaced.CloudID != "i-789" || replaced.SSHHost != "" || replaced.SSHPort != 0 || replaced.TailscaleIPv4 != "" || replaced.TailscaleFQDN != "" || replaced.BridgeURL != "" {
		t.Fatalf("replaced claim=%#v", replaced)
	}
	updated = replaced

	labels := cloneStringMap(updated.Labels)
	labels["state"] = "cleanup"
	labeled, err := updateLeaseClaimLabelsIfUnchanged(leaseID, updated, labels)
	if err != nil {
		t.Fatal(err)
	}
	if labeled.Labels["state"] != "cleanup" {
		t.Fatalf("labeled claim=%#v", labeled)
	}
	if _, err := updateLeaseClaimLabelsIfUnchanged(leaseID, updated, labels); err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("stale label update err=%v", err)
	}
	if empty, err := updateLeaseClaimLabelsIfUnchanged("", leaseClaim{}, nil); err != nil || empty.LeaseID != "" {
		t.Fatalf("empty label update=%#v err=%v", empty, err)
	}

	exact, ok, exactFile, err := resolveLeaseClaimForProviderWithExact(leaseID, "aws")
	if err != nil || !ok || !exactFile || exact.LeaseID != leaseID {
		t.Fatalf("exact claim=%#v ok=%v exact=%v err=%v", exact, ok, exactFile, err)
	}
	foreign, ok, exactFile, err := resolveLeaseClaimForProviderWithExact(leaseID, "gcp")
	if err != nil || ok || !exactFile || foreign.LeaseID != leaseID {
		t.Fatalf("foreign exact claim=%#v ok=%v exact=%v err=%v", foreign, ok, exactFile, err)
	}
	alias, ok, exactFile, err := resolveLeaseClaimForProviderWithExact(slug, "aws")
	if err != nil || !ok || exactFile || alias.LeaseID != leaseID {
		t.Fatalf("alias claim=%#v ok=%v exact=%v err=%v", alias, ok, exactFile, err)
	}
	if empty, ok, exactFile, err := resolveLeaseClaimForProviderWithExact("", "aws"); err != nil || ok || exactFile || empty.LeaseID != "" {
		t.Fatalf("empty exact claim=%#v ok=%v exact=%v err=%v", empty, ok, exactFile, err)
	}

	for identifier, want := range map[string]bool{
		"":            false,
		leaseID:       true,
		"i-789":       true,
		slug:          true,
		"not-a-match": false,
	} {
		if got := leaseClaimMatchesIdentifier(labeled, identifier); got != want {
			t.Fatalf("leaseClaimMatchesIdentifier(%q)=%v want %v", identifier, got, want)
		}
	}
	if exists, err := leaseClaimExists(leaseID); err != nil || !exists {
		t.Fatalf("existing claim exists=%v err=%v", exists, err)
	}
	if exists, err := leaseClaimExists("cbx_missingclaim123"); err != nil || exists {
		t.Fatalf("missing claim exists=%v err=%v", exists, err)
	}
	if exists, err := leaseClaimExists("../invalid"); err != nil || exists {
		t.Fatalf("invalid claim exists=%v err=%v", exists, err)
	}
}

func TestResolveLeaseClaimForProviderCloudIDRejectsDuplicates(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	for _, leaseID := range []string{"cbx_111111111111", "cbx_222222222222"} {
		server := Server{Provider: "aws", CloudID: "i-duplicate"}
		if err := claimLeaseTargetForRepoConfig(leaseID, leaseID, cfg, server, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, err := resolveLeaseClaimForProviderCloudID("i-duplicate", "aws"); err == nil || ok {
		t.Fatalf("duplicate cloud id lookup ok=%t err=%v", ok, err)
	}
}

func TestUpdateLeaseClaimTailscaleRecordsEndpointAndLabels(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repo")
	if err := claimLeaseForRepoProvider("isb_crabbox-node-a", "node-a", "islo", repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}

	if err := updateLeaseClaimTailscale("isb_crabbox-node-a", "100.64.2.20", "node-a.tail-scale.ts.net"); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim("isb_crabbox-node-a")
	if err != nil {
		t.Fatal(err)
	}
	if claim.TailscaleIPv4 != "100.64.2.20" || claim.TailscaleFQDN != "node-a.tail-scale.ts.net" {
		t.Fatalf("tailnet endpoint not recorded: %#v", claim)
	}
	if claim.Labels["tailscale"] != "true" || claim.Labels["tailscale_state"] != "ready" {
		t.Fatalf("tailscale labels missing: %#v", claim.Labels)
	}
	if claim.Labels["tailscale_ipv4"] != "100.64.2.20" || claim.Labels["tailscale_fqdn"] != "node-a.tail-scale.ts.net" {
		t.Fatalf("tailscale endpoint labels missing: %#v", claim.Labels)
	}

	// A second update with only the IPv4 must preserve the previously stored FQDN.
	if err := updateLeaseClaimTailscale("isb_crabbox-node-a", "100.64.2.21", ""); err != nil {
		t.Fatal(err)
	}
	claim, err = readLeaseClaim("isb_crabbox-node-a")
	if err != nil {
		t.Fatal(err)
	}
	if claim.TailscaleIPv4 != "100.64.2.21" || claim.TailscaleFQDN != "node-a.tail-scale.ts.net" {
		t.Fatalf("partial update should keep prior FQDN: %#v", claim)
	}
}

func TestUpdateLeaseClaimTailscaleIgnoresEmptyOrMissingClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// Empty lease ID is a no-op.
	if err := updateLeaseClaimTailscale("", "100.64.2.20", ""); err != nil {
		t.Fatalf("empty leaseID should be a no-op, got %v", err)
	}

	// A well-formed but unknown lease ID must not create a claim file.
	if err := updateLeaseClaimTailscale("isb_crabbox-missing", "100.64.2.20", "x.ts.net"); err != nil {
		t.Fatalf("missing claim should be a no-op, got %v", err)
	}
	claim, err := readLeaseClaim("isb_crabbox-missing")
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID != "" {
		t.Fatalf("missing claim should stay absent: %#v", claim)
	}
}

func TestLeaseClaimTailscaleSettingsSurviveEndpointClear(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", "islo", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	tags := []string{"tag:pond", "tag:ci"}
	if err := updateLeaseClaimTailscaleSettings(leaseID, "node-a", tags, "https://control.example", "100.64.0.1", true); err != nil {
		t.Fatal(err)
	}
	tags[0] = "tag:mutated"
	if err := updateLeaseClaimTailscale(leaseID, "100.64.2.20", "node-a.example.ts.net"); err != nil {
		t.Fatal(err)
	}
	if err := clearLeaseClaimTailscale(leaseID); err != nil {
		t.Fatal(err)
	}

	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.TailscaleIPv4 != "" || claim.TailscaleFQDN != "" {
		t.Fatalf("tailnet endpoint not cleared: %#v", claim)
	}
	if claim.TailscaleHostname != "node-a" ||
		strings.Join(claim.TailscaleTags, ",") != "tag:pond,tag:ci" ||
		claim.TailscaleLoginURL != "https://control.example" ||
		claim.TailscaleExitNode != "100.64.0.1" ||
		!claim.TailscaleExitLAN {
		t.Fatalf("recovery settings not preserved: %#v", claim)
	}
	for _, key := range []string{"tailscale", "tailscale_state", "tailscale_ipv4", "tailscale_fqdn"} {
		if _, ok := claim.Labels[key]; ok {
			t.Fatalf("stale label %q retained: %#v", key, claim.Labels)
		}
	}
}

func TestLeaseClaimTailscaleSettingsIgnoreEmptyOrMissingClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := updateLeaseClaimTailscaleSettings("", "node-a", []string{"tag:ci"}, "", "", false); err != nil {
		t.Fatalf("empty settings update: %v", err)
	}
	if err := clearLeaseClaimTailscale(""); err != nil {
		t.Fatalf("empty clear: %v", err)
	}
	if err := updateLeaseClaimTailscaleSettings("isb_crabbox-missing", "node-a", []string{"tag:ci"}, "", "", false); err != nil {
		t.Fatalf("missing settings update: %v", err)
	}
	if err := clearLeaseClaimTailscale("isb_crabbox-missing"); err != nil {
		t.Fatalf("missing clear: %v", err)
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

func TestClaimLeaseForRepoConfigIfUnchangedPreservesEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	leaseID := "cbx_owneronly123"
	server := Server{
		CloudID:  "i-owner-only",
		Provider: "aws",
		Labels:   map[string]string{"provider": "aws", "slug": "owner-only", "state": "ready"},
	}
	target := SSHTarget{Host: "192.0.2.130", Port: "22"}
	if err := claimLeaseTargetForRepoConfig(leaseID, "owner-only", cfg, server, target, "/repo-a", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	expected, expectedExists, err := readLeaseClaimWithPresence(leaseID)
	if err != nil || !expectedExists {
		t.Fatalf("claim=%#v exists=%v err=%v", expected, expectedExists, err)
	}
	claimed, err := claimLeaseForRepoConfigIfUnchanged(leaseID, "owner-only", cfg, "/repo-b", time.Hour, true, expected, true)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.RepoRoot != "/repo-b" || claimed.SSHHost != target.Host || claimed.SSHPort != 22 {
		t.Fatalf("claim=%#v", claimed)
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
