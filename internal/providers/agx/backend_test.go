package agx

import (
	"context"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func newTestBackend(cfg Config) *agxBackend {
	cfg.Provider = agxProvider
	return &agxBackend{spec: Provider{}.Spec(), cfg: cfg, rt: Runtime{Stdout: io.Discard, Stderr: io.Discard}}
}

func TestAGXSSHTargetUsesWorkspaceGatewayAndBYOKey(t *testing.T) {
	b := newTestBackend(Config{SSHKey: "/home/alice/.ssh/id_ed25519", AGX: AGXConfig{Workspace: "workspace.agx.so", User: "root"}})
	target := b.agxSSHTarget("blue-lobster")
	if target.User != "root+blue-lobster" {
		t.Fatalf("user=%q", target.User)
	}
	if target.Host != "workspace.agx.so" || target.Port != "22" {
		t.Fatalf("host/port=%q/%q", target.Host, target.Port)
	}
	if target.Key != "/home/alice/.ssh/id_ed25519" {
		t.Fatalf("expected BYO ssh key, got %q", target.Key)
	}
	if target.TargetOS != targetLinux || target.DisableHostKeyChecking {
		t.Fatalf("target=%#v", target)
	}
}

func TestAGXVMUserAndWorkspaceDefaults(t *testing.T) {
	b := newTestBackend(Config{})
	if b.vmUser() != defaultVMUser {
		t.Fatalf("vmUser=%q", b.vmUser())
	}
	if b.workspaceHost("") != defaultWorkspace {
		t.Fatalf("workspace=%q", b.workspaceHost(""))
	}
	custom := newTestBackend(Config{AGX: AGXConfig{User: "agent", Workspace: "eu.agx.so"}})
	if got := custom.agxSSHTarget("crab").User; got != "agent+crab" {
		t.Fatalf("user=%q", got)
	}
	if got := custom.workspaceHost(""); got != "eu.agx.so" {
		t.Fatalf("workspace=%q", got)
	}
}

func TestAGXInstanceNameIsStableSlug(t *testing.T) {
	if got := agxInstanceName("Blue Lobster"); got != normalizeLeaseSlug("Blue Lobster") {
		t.Fatalf("instance=%q", got)
	}
}

func TestCleanAGXWorkRootRejectsBroadPaths(t *testing.T) {
	for _, p := range []string{"/", "/home", "/root", "/tmp", "relative", ""} {
		if err := cleanAGXWorkRoot(p); err == nil {
			t.Fatalf("expected %q to be rejected", p)
		}
	}
	if err := cleanAGXWorkRoot("/root/crabbox"); err != nil {
		t.Fatalf("work root rejected: %v", err)
	}
}

func TestAGXRejectsTailscale(t *testing.T) {
	cfg := Config{AGX: AGXConfig{WorkRoot: "/root/crabbox"}}
	cfg.Tailscale.Enabled = true
	_, err := NewAGXBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "--tailscale is not supported for provider=agx") {
		t.Fatalf("err=%v", err)
	}
}

func TestAGXRejectsUnsafeWorkRootBeforeBackend(t *testing.T) {
	cfg := Config{AGX: AGXConfig{WorkRoot: "/tmp"}}
	_, err := NewAGXBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("err=%v", err)
	}
}

func TestAGXNeedsNoAPIKey(t *testing.T) {
	// AGX publishes no control-plane API; a backend must construct without any
	// token, authenticating purely with the operator's SSH key.
	cfg := Config{AGX: AGXConfig{WorkRoot: "/root/crabbox"}}
	if _, err := NewAGXBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		t.Fatalf("backend should build without an API key: %v", err)
	}
}

func TestResolveAndListUseLocalClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repoRoot := t.TempDir()
	b := newTestBackend(Config{SSHKey: "/key", AGX: AGXConfig{Workspace: "workspace.agx.so", User: "root", WorkRoot: "/root/crabbox"}})

	server := b.instanceServer("agx_swift-crab", "swift-crab", "swift-crab", "ready")
	target := b.agxSSHTarget("swift-crab")
	if err := claimLeaseTargetForRepoConfig("agx_swift-crab", "swift-crab", b.configForRun(), server, target, repoRoot, 0, true); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "swift-crab", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "agx_swift-crab" || lease.Server.CloudID != "swift-crab" {
		t.Fatalf("lease=%#v", lease)
	}

	views, err := b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].CloudID != "swift-crab" {
		t.Fatalf("views=%#v", views)
	}

	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, Force: true}); err != nil {
		t.Fatal(err)
	}
	views, err = b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 0 {
		t.Fatalf("expected no claims after release, got %#v", views)
	}
}

func TestResolveReleaseOnlyForUnknownIDSkipsSSH(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := newTestBackend(Config{SSHKey: "/key", AGX: AGXConfig{Workspace: "workspace.agx.so", User: "root", WorkRoot: "/root/crabbox"}})
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "lonely-crab", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "agx_lonely-crab" || lease.Server.CloudID != "lonely-crab" {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestAGXProviderRegistration(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != "agx" || spec.Kind != core.ProviderKindSSHLease {
		t.Fatalf("spec=%#v", spec)
	}
	if !spec.Features.Has(core.FeatureSSH) || !spec.Features.Has(core.FeatureCrabboxSync) {
		t.Fatalf("features=%#v", spec.Features)
	}
	if spec.Features.Has(core.FeatureCleanup) {
		t.Fatal("AGX has no control-plane API, so it must not advertise orphan cleanup")
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v", spec.Coordinator)
	}
}
