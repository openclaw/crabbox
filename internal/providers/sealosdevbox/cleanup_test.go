package sealosdevbox

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCleanupDryRunSkipsWrongScopeAndDoesNotMutate(t *testing.T) {
	cfg := lifecycleConfig()
	owned := cleanupDevboxJSON(cfg, "cbx_owned000000", "owned", "devbox-owned", sealosClaimScopeID(cfg), "2026-06-24T00:00:00Z")
	wrongScope := cleanupDevboxJSON(cfg, "cbx_wrong000000", "wrong", "devbox-wrong", "other-scope", "2026-06-24T00:00:00Z")
	notOwned := `{"metadata":{"name":"devbox-user","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"dashboard"},"annotations":{}},"status":{"state":"Shutdown"}}`
	runner := &lifecycleRunner{outputs: []string{`{"items":[` + owned + `,` + wrongScope + `,` + notOwned + `]}`}}
	var stdout, stderr bytes.Buffer
	backend := lifecycleBackend(cfg, runner)
	backend.rt.Stdout = &stdout
	backend.rt.Stderr = &stderr
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "dry_run=true") || !strings.Contains(stdout.String(), "devbox-owned") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if strings.Contains(stdout.String(), "devbox-wrong") || strings.Contains(stdout.String(), "devbox-user") {
		t.Fatalf("dry-run included wrong resource: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "outside active provider scope") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	for _, req := range runner.requests {
		if strings.Contains(strings.Join(req.Args, " "), " delete ") {
			t.Fatalf("dry-run deleted: %#v", runner.requests)
		}
	}
}

func TestCleanupDeletesExpiredOwnedDevboxAndRemovesClaimAndKey(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_owned000000"
	slug := "owned"
	name := "devbox-owned"
	server := releaseServer(cfg, leaseID, slug, name)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{Host: "ssh.sealos.example.test", Port: "2222"}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	keyPath, err := persistDevboxKey(leaseID, devboxSecretKeys{PublicKey: "ssh-ed25519 AAA test", PrivateKey: "private\n"})
	if err != nil {
		t.Fatal(err)
	}
	item := cleanupDevboxJSON(cfg, leaseID, slug, name, sealosClaimScopeID(cfg), "2026-06-24T00:00:00Z")
	runner := &lifecycleRunner{outputs: []string{
		`{"items":[` + item + `]}`,
		item,
		"deleted",
	}}
	var stdout bytes.Buffer
	backend := lifecycleBackend(cfg, runner)
	backend.rt.Stdout = &stdout
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stored key still exists or stat failed unexpectedly: %v", err)
	}
	got := strings.Join(flattenArgs(runner.requests), " ")
	if !strings.Contains(got, "delete "+devboxResource+"/"+name+" --ignore-not-found=true --preconditions=uid=uid-test") || !strings.Contains(stdout.String(), "reason=expired") {
		t.Fatalf("cleanup output=%q commands=%s", stdout.String(), got)
	}
}

func TestCleanupRefusesResourceWhoseScopeChangesBeforeDelete(t *testing.T) {
	cfg := lifecycleConfig()
	leaseID := "cbx_scopechange00"
	slug := "scope-change"
	name := "devbox-scope-change"
	owned := cleanupDevboxJSON(cfg, leaseID, slug, name, sealosClaimScopeID(cfg), "2026-06-24T00:00:00Z")
	changed := cleanupDevboxJSON(cfg, leaseID, slug, name, "other-scope", "2026-06-24T00:00:00Z")
	runner := &lifecycleRunner{outputs: []string{`{"items":[` + owned + `]}`, changed}}
	backend := lifecycleBackend(cfg, runner)

	err := backend.Cleanup(context.Background(), core.CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "provider scope changed") {
		t.Fatalf("cleanup error=%v", err)
	}
	if got := strings.Join(flattenArgs(runner.requests), " "); strings.Contains(got, " delete ") {
		t.Fatalf("scope change reached delete: %s", got)
	}
}

func TestCleanupRemovesOnlySameScopeStaleClaims(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	if err := core.ClaimLeaseForRepoProviderScope("cbx_stale000000", "stale", providerName, sealosClaimScope(cfg), t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	other := cfg
	other.SealosDevbox.SSHGatewayHost = "other-gateway.example.test"
	if err := core.ClaimLeaseForRepoProviderScope("cbx_other000000", "other", providerName, sealosClaimScope(other), t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunner{outputs: []string{`{"items":[]}`}}
	var stdout bytes.Buffer
	backend := lifecycleBackend(cfg, runner)
	backend.rt.Stdout = &stdout
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence("cbx_stale000000"); err != nil || exists {
		t.Fatalf("same-scope stale claim exists=%v err=%v", exists, err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence("cbx_other000000"); err != nil || !exists {
		t.Fatalf("other-scope claim exists=%v err=%v", exists, err)
	}
	if !strings.Contains(stdout.String(), "stale-claim lease=cbx_stale000000") || strings.Contains(stdout.String(), "cbx_other000000") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func cleanupDevboxJSON(cfg core.Config, leaseID, slug, name, scope, expiresAt string) string {
	return `{"metadata":{"name":"` + name + `","namespace":"` + cfg.SealosDevbox.Namespace + `","uid":"uid-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + scope + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"` + cfg.SealosDevbox.Namespace + `","crabbox.dev/expires_at":"` + expiresAt + `"}},"status":{"state":"Shutdown","phase":"Shutdown"}}`
}
