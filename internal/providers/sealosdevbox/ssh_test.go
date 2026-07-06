package sealosdevbox

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestSSHGateTargetUsesPublicKeyRouteDefaults(t *testing.T) {
	cfg := lifecycleConfig()
	backend := lifecycleBackend(cfg, &lifecycleRunner{})
	item := devboxItem{Metadata: devboxMeta{Name: "devbox-blue"}}
	target, err := backend.sshTarget(item, "/tmp/cbx-key", true)
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "ssh.sealos.example.test" || target.Port != "2233" || target.User != "devbox" || target.Key != "/tmp/cbx-key" {
		t.Fatalf("target=%#v", target)
	}
	if target.TargetOS != core.TargetLinux || target.NetworkKind != core.NetworkPublic {
		t.Fatalf("target OS/network=%#v", target)
	}
	if !strings.Contains(target.ReadyCheck, "command -v git") || !strings.Contains(target.ReadyCheck, "command -v rsync") || !strings.Contains(target.ReadyCheck, "command -v tar") {
		t.Fatalf("ready check=%q", target.ReadyCheck)
	}
	if strings.Contains(target.User, "@") {
		t.Fatalf("username-encoded routing became default: %#v", target)
	}
}

func TestPrepareSSHBootstrapsToolsBetweenReadinessChecks(t *testing.T) {
	backend := lifecycleBackend(lifecycleConfig(), &lifecycleRunner{})
	var checks []string
	backend.sshReady = func(_ context.Context, target *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		checks = append(checks, target.ReadyCheck)
		return nil
	}
	var command string
	backend.sshRun = func(_ context.Context, target core.SSHTarget, remote string) error {
		if target.Port != "2233" {
			t.Fatalf("bootstrap target=%#v", target)
		}
		command = remote
		return nil
	}
	target := core.SSHTarget{Host: "ssh.sealos.example.test", Port: "2233", User: "devbox", Key: "/tmp/key", ReadyCheck: sealosSSHReadyCheck}
	if err := backend.prepareSSH(context.Background(), &target, "Sealos DevBox SSH"); err != nil {
		t.Fatal(err)
	}
	if len(checks) != 2 || checks[0] != "true" || checks[1] != sealosSSHReadyCheck {
		t.Fatalf("readiness checks=%q", checks)
	}
	for _, want := range []string{"command -v rsync", "sudo -n", "apt-get install", "dnf install", "yum install", "apk add", sealosSSHReadyCheck} {
		if !strings.Contains(command, want) {
			t.Fatalf("bootstrap command missing %q:\n%s", want, command)
		}
	}
}

func TestNodePortTargetUsesStatusPortAndNodeHost(t *testing.T) {
	cfg := lifecycleConfig()
	cfg.SealosDevbox.Network = networkNodePort
	cfg.SealosDevbox.NodeHost = "node-1.example.test"
	backend := lifecycleBackend(cfg, &lifecycleRunner{})
	item := devboxItem{
		Metadata: devboxMeta{Name: "devbox-blue"},
		Status: devboxStatus{Network: map[string]any{
			"ports": []any{map[string]any{"name": "ssh", "nodePort": float64(32022)}},
		}},
	}
	target, err := backend.sshTarget(item, "/tmp/cbx-key", true)
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "node-1.example.test" || target.Port != "32022" || target.Key != "/tmp/cbx-key" {
		t.Fatalf("target=%#v", target)
	}
}

func TestNodePortTargetUsesSealosV1Alpha2NetworkStatus(t *testing.T) {
	cfg := lifecycleConfig()
	cfg.SealosDevbox.Network = networkNodePort
	cfg.SealosDevbox.NodeHost = "node.example.test"
	backend := lifecycleBackend(cfg, &lifecycleRunner{})
	item := devboxItem{
		Metadata: devboxMeta{Name: "devbox-blue"},
		Status:   devboxStatus{Network: map[string]any{"type": "NodePort", "nodePort": float64(32022)}},
	}
	target, err := backend.sshTarget(item, "/tmp/cbx-key", true)
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "node.example.test" || target.Port != "32022" {
		t.Fatalf("target=%#v", target)
	}
}

func TestNodePortTargetPrefersSSHPort(t *testing.T) {
	cfg := lifecycleConfig()
	cfg.SealosDevbox.Network = networkNodePort
	cfg.SealosDevbox.NodeHost = "node-1.example.test"
	backend := lifecycleBackend(cfg, &lifecycleRunner{})
	item := devboxItem{
		Metadata: devboxMeta{Name: "devbox-blue"},
		Status: devboxStatus{Network: map[string]any{
			"ports": []any{
				map[string]any{"name": "http", "port": float64(80), "nodePort": float64(30080)},
				map[string]any{"name": "ssh", "port": float64(22), "nodePort": float64(32022)},
			},
		}},
	}
	target, err := backend.sshTarget(item, "/tmp/cbx-key", true)
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "node-1.example.test" || target.Port != "32022" {
		t.Fatalf("target=%#v", target)
	}
}

func TestSSHTargetValidationRejectsInvalidRoute(t *testing.T) {
	cfg := lifecycleConfig()
	cfg.SealosDevbox.SSHUser = "dev box"
	backend := lifecycleBackend(cfg, &lifecycleRunner{})
	if _, err := backend.sshTarget(devboxItem{}, "/tmp/cbx-key", true); err == nil || !strings.Contains(err.Error(), "SSH user") {
		t.Fatalf("invalid user err=%v", err)
	}
	cfg = lifecycleConfig()
	cfg.SealosDevbox.Network = networkNodePort
	cfg.SealosDevbox.NodeHost = "node-1.example.test"
	backend = lifecycleBackend(cfg, &lifecycleRunner{})
	if _, err := backend.sshTarget(devboxItem{Metadata: devboxMeta{Name: "devbox-blue"}}, "/tmp/cbx-key", true); err == nil || !strings.Contains(err.Error(), "NodePort") {
		t.Fatalf("missing nodePort err=%v", err)
	}
	httpOnly := devboxItem{
		Metadata: devboxMeta{Name: "devbox-blue"},
		Status: devboxStatus{Network: map[string]any{
			"ports": []any{map[string]any{"name": "http", "port": float64(80), "nodePort": float64(30080)}},
		}},
	}
	if _, err := backend.sshTarget(httpOnly, "/tmp/cbx-key", true); err == nil || !strings.Contains(err.Error(), "NodePort") {
		t.Fatalf("http nodePort err=%v", err)
	}
	if _, err := backend.sshTarget(devboxItem{Status: devboxStatus{Network: map[string]any{"sshNodePort": float64(32022)}}}, "", true); err == nil || !strings.Contains(err.Error(), "key path") {
		t.Fatalf("missing key err=%v", err)
	}
}

func TestResolveWaitsForSSHBeforePersistingEndpoint(t *testing.T) {
	isolateSealosState(t)
	cfg := lifecycleConfig()
	leaseID := "cbx_123456abcdef"
	slug := "blue"
	name := core.LeaseProviderName(leaseID, slug)
	privateKey := "private"
	publicKey := "ssh-ed25519 AAA test"
	devboxJSON := `{"metadata":{"name":"` + name + `","namespace":"team-a","labels":{"app.kubernetes.io/managed-by":"crabbox","crabbox.dev/provider":"sealos-devbox","crabbox.dev/lease-id":"` + leaseID + `","crabbox.dev/slug":"blue"},"annotations":{"crabbox.dev/provider-scope":"` + sealosClaimScopeID(cfg) + `","crabbox.dev/devbox_name":"` + name + `","crabbox.dev/devbox_namespace":"team-a"}},"status":{"state":"Running","phase":"Running"}}`
	devboxJSON = withDevboxUID(devboxJSON)
	secretJSON := ownedDevboxSecretJSON(name, "team-a", "uid-test", publicKey, privateKey, false)
	runner := &lifecycleRunner{outputs: []string{
		`{"items":[` + devboxJSON + `]}`,
		secretJSON,
	}}
	called := false
	backend := lifecycleBackend(cfg, runner)
	backend.sshReady = func(_ context.Context, target *core.SSHTarget, _ io.Writer, phase string, _ time.Duration) error {
		called = true
		if target.Host != "ssh.sealos.example.test" || phase != "Sealos DevBox SSH" {
			t.Fatalf("wait target=%#v phase=%q", target, phase)
		}
		claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
		if err != nil {
			t.Fatal(err)
		}
		if !exists || claim.CloudID != devboxCloudID("team-a", name) || claim.ProviderScope != sealosClaimScope(cfg) {
			t.Fatalf("reclaim did not bind exact resource before SSH wait: %#v", claim)
		}
		if claim.SSHHost != "" || claim.SSHPort != 0 {
			t.Fatalf("endpoint persisted before SSH wait: %#v", claim)
		}
		return nil
	}
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: slug, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("SSH wait was not called")
	}
	if lease.SSH.Host != "ssh.sealos.example.test" || lease.SSH.Key == "" {
		t.Fatalf("lease=%#v", lease)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.SSHHost != "ssh.sealos.example.test" || claim.SSHPort != 2233 {
		t.Fatalf("claim endpoint=%#v", claim)
	}
}
