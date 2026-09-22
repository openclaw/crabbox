package azure

import (
	"context"
	"errors"
	"io"
	"maps"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func (c *fakeAzureClient) CreateFixedServer(ctx context.Context, cfg core.Config, publicKey, leaseID, slug string, labels map[string]string) (core.Server, error) {
	server, _, err := c.CreateServerWithFallback(ctx, cfg, publicKey, leaseID, slug, true, nil)
	if err == nil {
		server.Labels = maps.Clone(labels)
		c.created = server
		c.servers = append(c.servers, server)
		err = c.fixedReplyErr
	}
	return server, err
}

func fixedAzureTestBackend(t *testing.T, client *fakeAzureClient) *azureLeaseBackend {
	t.Helper()
	testutil.IsolateUserDirs(t)
	oldClient, oldCIDRs, oldBootstrap := newAzureClient, validateAzureSSHCIDRsForAcquire, bootstrapManagedWindowsDesktop
	t.Cleanup(func() {
		newAzureClient, validateAzureSSHCIDRsForAcquire, bootstrapManagedWindowsDesktop = oldClient, oldCIDRs, oldBootstrap
	})
	newAzureClient = func(context.Context, core.Config) (azureClient, error) { return client, nil }
	validateAzureSSHCIDRsForAcquire = func(context.Context, core.Config) error { return nil }
	bootstrapManagedWindowsDesktop = func(context.Context, core.Config, *core.SSHTarget, string, io.Writer) error { return nil }
	cfg := core.BaseConfig()
	cfg.Provider, cfg.Azure.Subscription, cfg.Azure.ResourceGroup = "azure", "test-sub", "rg"
	cfg.Azure.Location, cfg.TargetOS = "eastus", core.TargetLinux
	return NewAzureLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*azureLeaseBackend)
}

func TestFixedAzureLifecycle(t *testing.T) {
	client := &fakeAzureClient{}
	b := fixedAzureTestBackend(t, client)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123456", RequestedSlug: "fixed", Repo: core.Repo{Root: t.TempDir()}, Keep: true}
	first, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Server.ImmutableID != replay.Server.ImmutableID || len(client.createLeaseIDs) != 1 {
		t.Fatal("duplicate allocation")
	}
	changed := req
	changed.Keep = false
	if _, err := b.Acquire(t.Context(), changed); err == nil {
		t.Fatal("changed intent accepted")
	}
	lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.State != "released" {
		t.Fatalf("missing tombstone: %+v %v", claim, err)
	}
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("released ID recreated")
	}
	lease, err = b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(client.deleted) != 1 {
		t.Fatal("duplicate deletion")
	}
}

func TestFixedAzureReadinessRecoveryAndIdentity(t *testing.T) {
	client := &fakeAzureClient{waitErr: errors.New("reply lost")}
	b := fixedAzureTestBackend(t, client)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123457", RequestedSlug: "recover", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("expected readiness failure")
	}
	if len(client.deleted) != 0 {
		t.Fatal("ambiguous resource rolled back")
	}
	client.waitErr = nil
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if len(client.createLeaseIDs) != 1 {
		t.Fatal("duplicate create")
	}
	client.claimScope = "subscription:other|resource-group:rg"
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("account change accepted")
	}
	client.claimScope = ""
	client.servers[0].ImmutableID = "replacement"
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("replacement adopted")
	}
}

func TestFixedAzureAmbiguousCreateNeverResubmits(t *testing.T) {
	client := &fakeAzureClient{createErr: errors.New("reply lost")}
	b := fixedAzureTestBackend(t, client)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123458", RequestedSlug: "ambiguous", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("expected create failure")
	}
	client.createErr = nil
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("ambiguous create resubmitted")
	}
	if len(client.createLeaseIDs) != 1 {
		t.Fatal("duplicate create")
	}
}

func TestFixedAzureLostCreateReplyRecoversOriginalVM(t *testing.T) {
	client := &fakeAzureClient{fixedReplyErr: errors.New("reply lost")}
	b := fixedAzureTestBackend(t, client)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123459", RequestedSlug: "lost-reply", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("expected lost response")
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.CloudImmutableID != "" {
		t.Fatalf("unexpected response identity: %+v %v", claim, err)
	}
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.ImmutableID != client.created.ImmutableID || len(client.createLeaseIDs) != 1 {
		t.Fatal("lost reply created replacement")
	}
}

func TestFixedAzureBindsLeaseMetadata(t *testing.T) {
	client := &fakeAzureClient{}
	b := fixedAzureTestBackend(t, client)
	b.Cfg.Tailscale.Enabled = true
	b.Cfg.Tailscale.AuthKey = "test-only-key"
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123460", RequestedSlug: "metadata", Repo: core.Repo{Root: t.TempDir()}}
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Labels["tailscale_hostname"] == "" {
		t.Fatal("generated Tailscale hostname missing from lease labels")
	}
	b.Cfg.ExposedPorts = []string{"8080"}
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("changed published ports accepted")
	}
	if len(client.createLeaseIDs) != 1 {
		t.Fatal("duplicate create")
	}
}
