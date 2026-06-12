package ssh

import (
	"context"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStaticSSHDoctorDoesNotReportProbeWhenUnchecked(t *testing.T) {
	cfg := Config{}
	cfg.Static.Host = "example.test"
	backend := NewStaticSSHLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stderr: io.Discard}).(*staticLeaseBackend)

	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Message, "api=static_config") || strings.Contains(result.Message, "api=ssh_probe") {
		t.Fatalf("result=%#v", result)
	}
	if !strings.Contains(result.Message, "runtime=unchecked") {
		t.Fatalf("result=%#v", result)
	}
}

func TestStaticSSHRequestedSlugPersistsThroughClaimedResolveAndList(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldWait := waitForSSH
	waitForSSH = func(context.Context, *SSHTarget, io.Writer) error { return nil }
	t.Cleanup(func() { waitForSSH = oldWait })

	cfg := Config{Provider: "ssh"}
	cfg.TargetOS = "macos"
	cfg.SSHUser = "fallback-user"
	cfg.SSHPort = "2222"
	cfg.Static.Host = "static.example.test"
	cfg.Static.ID = "static_stable"
	cfg.Static.Name = "configured-name"
	cfg.Static.WorkRoot = "/workspace/static"
	backend := NewStaticSSHLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stderr: io.Discard}).(*staticLeaseBackend)
	lease, err := backend.Acquire(context.Background(), AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "requested-name",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Name != "requested-name" || serverSlug(lease.Server) != "requested-name" {
		t.Fatalf("acquired name=%q slug=%q, want requested-name", lease.Server.Name, serverSlug(lease.Server))
	}
	claim, ok, err := core.ResolveLeaseClaim(lease.LeaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.StaticHost != "static.example.test" || claim.StaticUser != "" || claim.StaticPort != "" || claim.StaticWorkRoot != "/workspace/static" {
		t.Fatalf("claim did not persist static target details: %#v", claim)
	}

	backend = NewStaticSSHLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stderr: io.Discard}).(*staticLeaseBackend)
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != "requested-name" || serverSlug(views[0]) != "requested-name" {
		t.Fatalf("views=%#v, want requested-name from persisted claim", views)
	}
	defaultLease, err := backend.Resolve(context.Background(), ResolveRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if defaultLease.Server.Name != "requested-name" || serverSlug(defaultLease.Server) != "requested-name" {
		t.Fatalf("default resolve server=%#v, want requested-name", defaultLease.Server)
	}
	byID, err := backend.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if byID.Server.Name != "requested-name" || serverSlug(byID.Server) != "requested-name" {
		t.Fatalf("resolve by id server=%#v, want requested-name", byID.Server)
	}
	if byID.SSH.Host != "static.example.test" || byID.SSH.User != "fallback-user" || byID.SSH.Port != "2222" {
		t.Fatalf("resolve by id ssh=%#v, want original claimed static target", byID.SSH)
	}
	bySlug, err := backend.Resolve(context.Background(), ResolveRequest{ID: "requested-name"})
	if err != nil {
		t.Fatal(err)
	}
	if bySlug.LeaseID != lease.LeaseID || bySlug.Server.Name != "requested-name" {
		t.Fatalf("resolve by slug lease=%#v, want lease=%s requested-name", bySlug, lease.LeaseID)
	}

	overrideCfg := cfg
	overrideCfg.SSHUser = "override-user"
	overrideCfg.SSHPort = "2200"
	backend = NewStaticSSHLeaseBackend(Provider{}.Spec(), overrideCfg, Runtime{Stderr: io.Discard}).(*staticLeaseBackend)
	override, err := backend.Resolve(context.Background(), ResolveRequest{ID: "requested-name"})
	if err != nil {
		t.Fatal(err)
	}
	if override.Server.Name != "requested-name" || override.SSH.User != "override-user" || override.SSH.Port != "2200" {
		t.Fatalf("override resolve=%#v, want requested-name with explicit static user/port", override)
	}
}

func TestStaticSSHRequestedSlugAvoidsClaimCollision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldWait := waitForSSH
	waitForSSH = func(context.Context, *SSHTarget, io.Writer) error { return nil }
	t.Cleanup(func() { waitForSSH = oldWait })
	if err := core.ClaimLeaseForRepoProvider("cbx_other123456", "requested-name", "aws", t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}

	cfg := Config{Provider: "ssh"}
	cfg.Static.Host = "static.example.test"
	backend := NewStaticSSHLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stderr: io.Discard}).(*staticLeaseBackend)
	lease, err := backend.Acquire(context.Background(), AcquireRequest{
		Repo:          core.Repo{Root: t.TempDir()},
		RequestedSlug: "requested-name",
	})
	if err != nil {
		t.Fatal(err)
	}
	slug := serverSlug(lease.Server)
	if slug == "requested-name" || !strings.HasPrefix(slug, "requested-name-") {
		t.Fatalf("slug=%q, want collision-suffixed requested-name", slug)
	}
	claim, ok, err := core.ResolveLeaseClaim(slug)
	if err != nil || !ok || claim.LeaseID != lease.LeaseID {
		t.Fatalf("claim=%#v ok=%t err=%v, want static lease claim by suffixed slug", claim, ok, err)
	}
}
