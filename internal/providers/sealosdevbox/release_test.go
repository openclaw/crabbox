package sealosdevbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestReleaseRetainsLeaseByPausingAndClearingEndpoint(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_123456abcdef"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	server := releaseServer(cfg, leaseID, slug, name)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{Host: "ssh.sealos.example.test", Port: "2222"}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunner{outputs: []string{
		releaseDevboxJSON(cfg, leaseID, slug, name),
		"patched",
	}}
	backend := lifecycleBackend(cfg, runner)
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.SSHHost != "" || claim.SSHPort != 0 || claim.Labels["state"] != "released" || claim.Labels["release"] != "pause" {
		t.Fatalf("claim=%#v", claim)
	}
	got := strings.Join(flattenArgs(runner.requests), " ")
	if !strings.Contains(got, "patch "+devboxResource+"/"+name) || !strings.Contains(got, `"state":"Paused"`) || !strings.Contains(got, `"resourceVersion":"rv-test"`) {
		t.Fatalf("release did not pause devbox: %s", got)
	}
	assertReleasePatchUsesScopeFingerprint(t, cfg, runner)
	if !backend.RetainLeaseClaimAfterRelease(core.LeaseTarget{LeaseID: leaseID, Server: server}) {
		t.Fatal("retained release should retain local claim")
	}
}

func TestReleaseRetainsLegacyRawScopeDevboxMigratesScopeAnnotation(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_legacyraw"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	server := releaseServer(cfg, leaseID, slug, name)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{Host: "ssh.sealos.example.test", Port: "2222"}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	legacyItem := `{"metadata":{"name":"` + name + `","namespace":"` + cfg.SealosDevbox.Namespace + `","uid":"uid-test","resourceVersion":"rv-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider_scope":"` + sealosClaimScope(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"` + cfg.SealosDevbox.Namespace + `"}},"status":{"state":"Running","phase":"Running"}}`
	runner := &lifecycleRunner{outputs: []string{
		legacyItem,
		"patched",
	}}
	backend := lifecycleBackend(cfg, runner)
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	assertReleasePatchUsesScopeFingerprint(t, cfg, runner)
}

func TestReleaseDeleteRemovesDevboxClaimAndKeyAfterValidation(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	cfg.SealosDevbox.DeleteOnRelease = true
	leaseID := "cbx_abcdef123456"
	slug := "red"
	name := core.LeaseProviderName(leaseID, slug)
	server := releaseServer(cfg, leaseID, slug, name)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{Host: "ssh.sealos.example.test", Port: "2222"}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	keys := devboxSecretKeys{PublicKey: "ssh-ed25519 AAA test", PrivateKey: "private\n"}
	keyPath, err := persistDevboxKey(leaseID, keys)
	if err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunner{outputs: []string{
		releaseDevboxJSON(cfg, leaseID, slug, name),
		"shutdown",
		"deleted",
	}}
	backend := lifecycleBackend(cfg, runner)
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stored key still exists or stat failed unexpectedly: %v", err)
	}
	got := strings.Join(flattenArgs(runner.requests), " ")
	if !strings.Contains(got, `"state":"Shutdown"`) || !strings.Contains(got, "delete "+devboxResource+"/"+name+" --ignore-not-found=true --preconditions=uid=uid-test") {
		t.Fatalf("delete release commands=%s", got)
	}
	if backend.RetainLeaseClaimAfterRelease(core.LeaseTarget{LeaseID: leaseID, Server: server}) {
		t.Fatal("delete release should not retain local claim")
	}
}

func TestReleaseDeleteKeepsLocalStateWhenKubectlMissing(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	cfg.SealosDevbox.DeleteOnRelease = true
	leaseID := "cbx_missingkubectl"
	slug := "red"
	name := core.LeaseProviderName(leaseID, slug)
	server := releaseServer(cfg, leaseID, slug, name)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{Host: "ssh.sealos.example.test", Port: "2222"}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	keyPath, err := persistDevboxKey(leaseID, devboxSecretKeys{PublicKey: "ssh-ed25519 AAA test", PrivateKey: "private\n"})
	if err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunner{
		stderrs:  []string{`exec: "kubectl": executable file not found in $PATH`},
		exitCode: []int{127},
		errors:   []error{errors.New(`exec: "kubectl": executable file not found in $PATH`)},
	}
	backend := lifecycleBackend(cfg, runner)
	err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}})
	if err == nil || !strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("release error=%v", err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || !exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key was removed after local kubectl failure: %v", err)
	}
	if got := strings.Join(flattenArgs(runner.requests), " "); strings.Contains(got, "patch ") || strings.Contains(got, "delete ") {
		t.Fatalf("local kubectl failure mutated resource: %s", got)
	}
}

func TestReleaseDeleteRemovesLocalStateWhenDevboxNotFound(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	cfg.SealosDevbox.DeleteOnRelease = true
	leaseID := "cbx_missingdevbox"
	slug := "red"
	name := core.LeaseProviderName(leaseID, slug)
	server := releaseServer(cfg, leaseID, slug, name)
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, slug, cfg, server, core.SSHTarget{Host: "ssh.sealos.example.test", Port: "2222"}, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	keyPath, err := persistDevboxKey(leaseID, devboxSecretKeys{PublicKey: "ssh-ed25519 AAA test", PrivateKey: "private\n"})
	if err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunner{
		stderrs:  []string{`Error from server (NotFound): devboxes.devbox.sealos.io "` + name + `" not found`},
		exitCode: []int{1},
		errors:   []error{errors.New("exit status 1")},
	}
	backend := lifecycleBackend(cfg, runner)
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stored key still exists or stat failed unexpectedly: %v", err)
	}
}

func TestResolveReleaseOnlyAllowsDeleteCleanupWhenDevboxMissing(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	cfg.SealosDevbox.DeleteOnRelease = true
	leaseID := "cbx_missingresolve"
	slug := "red"
	name := core.LeaseProviderName(leaseID, slug)
	server := releaseServer(cfg, leaseID, slug, name)
	runner := &lifecycleRunner{
		stderrs: []string{
			`Error from server (NotFound): devboxes.devbox.sealos.io "` + name + `" not found`,
			`Error from server (NotFound): devboxes.devbox.sealos.io "` + name + `" not found`,
		},
		exitCode: []int{1, 1},
		errors:   []error{errors.New("exit status 1"), errors.New("exit status 1")},
	}
	backend := lifecycleBackend(cfg, runner)
	if err := backend.claimLeaseForRepo(leaseID, slug, t.TempDir(), cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	if err := core.UpdateLeaseClaimEndpoint(leaseID, server, core.SSHTarget{Host: "ssh.sealos.example.test", Port: "2222"}); err != nil {
		t.Fatal(err)
	}
	keyPath, err := persistDevboxKey(leaseID, devboxSecretKeys{PublicKey: "ssh-ed25519 AAA test", PrivateKey: "private\n"})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID || lease.Server.Name != name {
		t.Fatalf("lease=%#v", lease)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stored key still exists or stat failed unexpectedly: %v", err)
	}
}

func TestReleaseRejectsIdentityMismatchBeforeMutation(t *testing.T) {
	cfg := lifecycleConfig()
	leaseID := "cbx_123456abcdef"
	name := core.LeaseProviderName(leaseID, "blue")
	runner := &lifecycleRunner{outputs: []string{
		releaseDevboxJSON(cfg, "cbx_other000000", "blue", name),
	}}
	backend := lifecycleBackend(cfg, runner)
	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: leaseID,
		Server:  releaseServer(cfg, leaseID, "blue", name),
	}})
	if err == nil || !strings.Contains(err.Error(), "lease identity changed") {
		t.Fatalf("err=%v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("identity mismatch mutated resource: %s", strings.Join(flattenArgs(runner.requests), " "))
	}
}

func releaseServer(cfg core.Config, leaseID, slug, name string) core.Server {
	item := devboxItem{}
	item.Metadata.Name = name
	item.Metadata.Namespace = cfg.SealosDevbox.Namespace
	item.Metadata.Labels = map[string]string{
		managedByLabel:     "crabbox",
		providerLabel:      providerName,
		leaseIDLabel:       leaseID,
		slugLabel:          slug,
		providerScopeLabel: sealosClaimScopeLabel(cfg),
	}
	item.Metadata.Annotations = map[string]string{
		annotationBase + "provider-scope":   sealosClaimScopeID(cfg),
		annotationBase + "devbox_name":      name,
		annotationBase + "devbox_namespace": cfg.SealosDevbox.Namespace,
	}
	return (&backend{cfg: cfg}).serverFromDevbox(item)
}

func releaseDevboxJSON(cfg core.Config, leaseID, slug, name string) string {
	return `{"metadata":{"name":"` + name + `","namespace":"` + cfg.SealosDevbox.Namespace + `","uid":"uid-test","resourceVersion":"rv-test","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"` + cfg.SealosDevbox.Namespace + `"}},"status":{"state":"Running","phase":"Running"}}`
}

func releasePatchPayload(t *testing.T, runner *lifecycleRunner) map[string]any {
	t.Helper()
	for _, req := range runner.requests {
		for i, arg := range req.Args {
			if arg != "-p" || i+1 >= len(req.Args) {
				continue
			}
			var patch map[string]any
			if err := json.Unmarshal([]byte(req.Args[i+1]), &patch); err != nil {
				t.Fatalf("patch payload is invalid JSON: %v", err)
			}
			return patch
		}
	}
	t.Fatalf("patch payload not found in %#v", runner.requests)
	return nil
}

func assertReleasePatchUsesScopeFingerprint(t *testing.T, cfg core.Config, runner *lifecycleRunner) {
	t.Helper()
	patch := releasePatchPayload(t, runner)
	metadata := patch["metadata"].(map[string]any)
	annotations := metadata["annotations"].(map[string]any)
	if annotations[annotationBase+"provider-scope"] != sealosClaimScopeID(cfg) {
		t.Fatalf("scope fingerprint annotation=%#v", annotations[annotationBase+"provider-scope"])
	}
	if annotations[annotationBase+"provider_scope_id"] != sealosClaimScopeID(cfg) {
		t.Fatalf("scope id annotation=%#v", annotations[annotationBase+"provider_scope_id"])
	}
	if raw, ok := annotations[annotationBase+"provider_scope"]; !ok || raw != nil {
		t.Fatalf("raw scope annotation should be removed, got %#v in %#v", raw, annotations)
	}
	for _, key := range []string{"gateway_host", "gateway_port", "node_host"} {
		if raw, ok := annotations[annotationBase+key]; !ok || raw != nil {
			t.Fatalf("raw route annotation %s should be removed, got %#v in %#v", key, raw, annotations)
		}
	}
	for key, value := range annotations {
		if value == sealosClaimScope(cfg) {
			t.Fatalf("raw provider scope leaked through annotation %s", key)
		}
		for _, rawRoute := range []string{cfg.SealosDevbox.SSHGatewayHost, cfg.SealosDevbox.SSHGatewayPort, cfg.SealosDevbox.NodeHost} {
			if strings.TrimSpace(rawRoute) != "" && value == strings.TrimSpace(rawRoute) {
				t.Fatalf("raw route value leaked through annotation %s", key)
			}
		}
	}
}
