package sealosdevbox

import (
	"context"
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
	if !strings.Contains(got, "patch "+devboxResource+"/"+name) || !strings.Contains(got, `"state":"Paused"`) {
		t.Fatalf("release did not pause devbox: %s", got)
	}
	if !backend.RetainLeaseClaimAfterRelease(core.LeaseTarget{LeaseID: leaseID, Server: server}) {
		t.Fatal("retained release should retain local claim")
	}
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
	if !strings.Contains(got, `"state":"Shutdown"`) || !strings.Contains(got, "delete "+devboxResource+"/"+name) {
		t.Fatalf("delete release commands=%s", got)
	}
	if backend.RetainLeaseClaimAfterRelease(core.LeaseTarget{LeaseID: leaseID, Server: server}) {
		t.Fatal("delete release should not retain local claim")
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
		providerScopeLabel: labelValueHash(sealosClaimScope(cfg)),
	}
	item.Metadata.Annotations = map[string]string{
		annotationBase + "provider_scope":   sealosClaimScope(cfg),
		annotationBase + "devbox_name":      name,
		annotationBase + "devbox_namespace": cfg.SealosDevbox.Namespace,
	}
	return (&backend{cfg: cfg}).serverFromDevbox(item)
}

func releaseDevboxJSON(cfg core.Config, leaseID, slug, name string) string {
	return `{"metadata":{"name":"` + name + `","namespace":"` + cfg.SealosDevbox.Namespace + `","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"` + slug + `"},"annotations":{"crabbox.dev/provider_scope":"` + sealosClaimScope(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"` + cfg.SealosDevbox.Namespace + `"}},"status":{"state":"Running","phase":"Running"}}`
}
