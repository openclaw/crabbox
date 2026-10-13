package sealosdevbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCleanupDryRunSkipsWrongScopeAndDoesNotMutate(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	claimExactSealosTarget(t, cfg, "cbx_owned000000", "owned", "devbox-owned", t.TempDir(), core.SSHTarget{})
	owned := cleanupDevboxJSON(cfg, "cbx_owned000000", "owned", "devbox-owned", sealosClaimScopeID(cfg), "2026-06-24T00:00:00Z")
	unclaimed := cleanupDevboxJSON(cfg, "cbx_unclaimed00", "unclaimed", "devbox-unclaimed", sealosClaimScopeID(cfg), "2026-06-24T00:00:00Z")
	wrongScope := cleanupDevboxJSON(cfg, "cbx_wrong000000", "wrong", "devbox-wrong", "other-scope", "2026-06-24T00:00:00Z")
	notOwned := `{"metadata":{"name":"devbox-user","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"dashboard"},"annotations":{}},"status":{"state":"Shutdown"}}`
	runner := &lifecycleRunner{outputs: []string{`{"items":[` + owned + `,` + unclaimed + `,` + wrongScope + `,` + notOwned + `]}`}}
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
	if strings.Contains(stdout.String(), "devbox-unclaimed") || strings.Contains(stdout.String(), "devbox-wrong") || strings.Contains(stdout.String(), "devbox-user") {
		t.Fatalf("dry-run included wrong resource: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "missing exact local claim") || !strings.Contains(stderr.String(), "outside active provider scope") {
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
	claimExactSealosTarget(t, cfg, leaseID, slug, name, t.TempDir(), core.SSHTarget{Host: "ssh.sealos.example.test", Port: "2222"})
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
	if !strings.Contains(stdout.String(), "reason=expired") {
		t.Fatalf("cleanup output=%q commands=%s", stdout.String(), got)
	}
	assertPreconditionedDevboxDelete(t, cfg, runner, name)
}

func TestCleanupRefusesResourceWhoseScopeChangesBeforeDelete(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_scopechange00"
	slug := "scope-change"
	name := "devbox-scope-change"
	claimExactSealosTarget(t, cfg, leaseID, slug, name, t.TempDir(), core.SSHTarget{})
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

func TestCleanupPreservesRenewedDevbox(t *testing.T) {
	for _, duringRevalidation := range []bool{false, true} {
		t.Run(fmt.Sprintf("during revalidation=%t", duringRevalidation), func(t *testing.T) {
			isolateSealosState(t)
			cfg := lifecycleConfig()
			leaseID, slug, name := "cbx_renewed0000", "renewed", "devbox-renewed"
			claimExactSealosTarget(t, cfg, leaseID, slug, name, t.TempDir(), core.SSHTarget{})
			old := cleanupDevboxJSON(cfg, leaseID, slug, name, sealosClaimScopeID(cfg), "2026-06-24T00:00:00Z")
			fresh := cleanupDevboxJSON(cfg, leaseID, slug, name, sealosClaimScopeID(cfg), "2026-06-24T13:00:00Z")
			fresh = strings.Replace(fresh, `"annotations":{`, `"annotations":{"crabbox.dev/keep":"true",`, 1)
			listed := fresh
			if duringRevalidation {
				listed = old
			}
			runner := &lifecycleRunner{outputs: []string{`{"items":[` + listed + `]}`, fresh}}
			backend := lifecycleBackend(cfg, runner)
			err := backend.Cleanup(context.Background(), core.CleanupRequest{})
			if duringRevalidation && (err == nil || !strings.Contains(err.Error(), "not expired")) {
				t.Fatalf("renewal error=%v", err)
			}
			if !duringRevalidation && err != nil {
				t.Fatal(err)
			}
			for _, req := range runner.requests {
				if strings.Contains(strings.Join(req.Args, " "), " delete ") {
					t.Fatal("renewed devbox was deleted")
				}
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || !exists {
				t.Fatalf("claim exists=%t err=%v", exists, err)
			}
		})
	}
}

func TestCleanupOwnershipUsesNativeLabels(t *testing.T) {
	cfg := lifecycleConfig()
	backend := lifecycleBackend(cfg, &lifecycleRunner{})
	for _, managedBy := range []string{"crabbox", "dashboard", ""} {
		t.Run("managed by "+managedBy, func(t *testing.T) {
			data := cleanupDevboxJSON(cfg, "cbx_owned000000", "owned", "devbox-owned", sealosClaimScopeID(cfg), "2026-06-24T00:00:00Z")
			data = strings.Replace(data, `"app.kubernetes.io/managed-by":"crabbox"`, `"app.kubernetes.io/managed-by":"`+managedBy+`"`, 1)
			data = strings.Replace(data, `"annotations":{`, `"annotations":{"crabbox.dev/crabbox":"true",`, 1)
			var item devboxItem
			if err := json.Unmarshal([]byte(data), &item); err != nil {
				t.Fatal(err)
			}
			eligible, reason := core.ShouldCleanupServer(backend.serverFromDevbox(item), backend.now())
			if eligible != (managedBy == "crabbox") {
				t.Fatalf("eligible=%t reason=%s", eligible, reason)
			}
		})
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
	runner := &lifecycleRunner{
		outputs: []string{`{"items":[]}`, "", ""},
		stderrs: []string{"",
			`Error from server (NotFound): devboxes.devbox.sealos.io "devbox-stale" not found`,
			`Error from server (NotFound): devboxes.devbox.sealos.io "devbox-stale" not found`,
		},
		exitCode: []int{0, 1, 1},
		errors:   []error{nil, errors.New("exit status 1"), errors.New("exit status 1")},
	}
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

func TestCleanupPreservesClaimWhenRemoteIdentityDrifts(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_drift000000"
	slug := "drift"
	name := "devbox-drift"
	claimExactSealosTarget(t, cfg, leaseID, slug, name, t.TempDir(), core.SSHTarget{})
	drifted := cleanupDevboxJSON(cfg, leaseID, slug, name, "other-scope", "2026-06-24T00:00:00Z")
	runner := &lifecycleRunner{outputs: []string{`{"items":[` + drifted + `]}`}}
	var stdout bytes.Buffer
	backend := lifecycleBackend(cfg, runner)
	backend.rt.Stdout = &stdout
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || !exists {
		t.Fatalf("drifted resource claim exists=%v err=%v", exists, err)
	}
	if strings.Contains(stdout.String(), "stale-claim") {
		t.Fatalf("cleanup erased recovery claim for present drifted resource: %q", stdout.String())
	}
}

func TestCleanupPreservesClaimWhenManagedLabelDrifts(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_labeldrift00"
	slug := "label-drift"
	name := core.LeaseProviderName(leaseID, slug)
	if err := core.ClaimLeaseForRepoProviderScope(leaseID, slug, providerName, sealosClaimScope(cfg), t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	drifted := strings.Replace(cleanupDevboxJSON(cfg, leaseID, slug, name, sealosClaimScopeID(cfg), "2026-06-24T00:00:00Z"), `"app.kubernetes.io/managed-by":"crabbox"`, `"app.kubernetes.io/managed-by":"dashboard"`, 1)
	runner := &lifecycleRunner{outputs: []string{`{"items":[]}`, drifted}}
	var stdout bytes.Buffer
	backend := lifecycleBackend(cfg, runner)
	backend.rt.Stdout = &stdout

	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || !exists {
		t.Fatalf("label-drifted resource claim exists=%v err=%v", exists, err)
	}
	if strings.Contains(stdout.String(), "stale-claim") {
		t.Fatalf("cleanup erased recovery claim for label-drifted resource: %q", stdout.String())
	}
	if got := strings.Join(flattenArgs(runner.requests), " "); !strings.Contains(got, "get "+devboxResource+"/"+name) {
		t.Fatalf("cleanup did not verify filtered resource directly: %s", got)
	}
}

func TestCleanupPreservesClaimWhenDirectAbsenceCheckFails(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_checkfail000"
	slug := "check-fail"
	if err := core.ClaimLeaseForRepoProviderScope(leaseID, slug, providerName, sealosClaimScope(cfg), t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunner{
		outputs:  []string{`{"items":[]}`, ""},
		stderrs:  []string{"", "forbidden"},
		exitCode: []int{0, 1},
		errors:   []error{nil, errors.New("exit status 1")},
	}
	backend := lifecycleBackend(cfg, runner)

	err := backend.Cleanup(context.Background(), core.CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("cleanup error=%v", err)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || !exists {
		t.Fatalf("failed absence check removed claim exists=%v err=%v", exists, readErr)
	}
}

func cleanupDevboxJSON(cfg core.Config, leaseID, slug, name, scope, expiresAt string) string {
	return `{"metadata":{"name":"` + name + `","namespace":"` + cfg.SealosDevbox.Namespace + `","uid":"uid-test","resourceVersion":"rv-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + scope + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"` + cfg.SealosDevbox.Namespace + `","crabbox.dev/expires_at":"` + expiresAt + `"}},"status":{"state":"Shutdown","phase":"Shutdown"}}`
}
