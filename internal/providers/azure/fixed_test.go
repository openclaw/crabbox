package azure

import (
	"context"
	"errors"
	"io"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestFixedAzureExplicitRecoveryStopsAfterLocalClaimLoss(t *testing.T) {
	client := &fakeAzureClient{}
	b := fixedAzureTestBackend(t, client)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123461", RequestedSlug: "restart-recovery", Repo: core.Repo{Root: t.TempDir()}}
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.RemoveLeaseClaimIfUnchanged(req.RequestedLeaseID, claim); err != nil {
		t.Fatal(err)
	}
	if err := b.ReclaimAndStop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatal(err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != lease.Server.CloudID {
		t.Fatalf("deleted=%v, want %s", client.deleted, lease.Server.CloudID)
	}
	terminal, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || terminal.FixedCreateIntent == nil || terminal.FixedCreateIntent.State != "released" {
		t.Fatalf("terminal=%+v err=%v", terminal, err)
	}
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("recovered single-use lease was recreated")
	}
	client.servers = nil
	client.listErr = errors.New("terminal replay must not require VM inventory")
	if err := b.ReclaimAndStop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatalf("terminal recovery retry: %v", err)
	}
	after, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || !reflect.DeepEqual(after, terminal) || len(client.deleted) != 1 {
		t.Fatalf("terminal retry changed receipt or repeated deletion: err=%v deleted=%v", err, client.deleted)
	}
	client.claimScope = "subscription:other|resource-group:rg"
	if err := b.ReclaimAndStop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err == nil || !strings.Contains(err.Error(), "account scope changed") {
		t.Fatalf("terminal retry accepted another account: %v", err)
	}
}

func TestFixedAzureExplicitRecoveryResumesInterruptedCleanup(t *testing.T) {
	client := &fakeAzureClient{}
	b := fixedAzureTestBackend(t, client)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123463", RequestedSlug: "restart-retry", Repo: core.Repo{Root: t.TempDir()}}
	lease, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.RemoveLeaseClaimIfUnchanged(req.RequestedLeaseID, claim); err != nil {
		t.Fatal(err)
	}
	cleanupLabels := map[string]string{
		core.AzureCleanupBindingLabel:         "v1",
		"_crabbox_azure_cleanup_nic_id":       "original-nic",
		"_crabbox_azure_cleanup_public_ip_id": "original-ip",
		"_crabbox_azure_cleanup_disk_id":      "original-disk",
	}
	client.prepareFunc = func(server core.Server) core.Server {
		server.Labels = maps.Clone(server.Labels)
		maps.Copy(server.Labels, cleanupLabels)
		return server
	}
	interrupted := errors.New("VM deleted; companion cleanup interrupted")
	client.deleteOwnedFunc = func(server core.Server) error {
		stored, err := core.ReadLeaseClaim(req.RequestedLeaseID)
		if err != nil || stored.CloudImmutableID != lease.Server.ImmutableID {
			t.Fatalf("recovery did not persist VM identity before deletion: %v", err)
		}
		for key, value := range cleanupLabels {
			if stored.Labels[key] != value || server.Labels[key] != value {
				t.Fatalf("cleanup identity %s was not retained before deletion", key)
			}
		}
		client.servers = nil
		return interrupted
	}
	t.Chdir(req.Repo.Root)
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "azure")
	t.Setenv("CRABBOX_COORDINATOR", "")
	app := core.App{Stdout: io.Discard, Stderr: io.Discard}
	args := []string{"stop", "--force", "--provider", "azure", "--id", req.RequestedLeaseID}
	if err := app.Run(t.Context(), args); !errors.Is(err, interrupted) {
		t.Fatalf("first recovery: %v, want interrupted companion cleanup", err)
	}
	client.prepareFunc = nil // The retry must use persisted identities, not recapture them.
	client.deleteOwnedFunc = nil
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("recovery retry after VM deletion: %v", err)
	}
	if len(client.ownedExpected) != 2 || len(client.deleted) != 1 {
		t.Fatalf("delete attempts=%d completed=%v", len(client.ownedExpected), client.deleted)
	}
	for key, value := range cleanupLabels {
		if client.ownedExpected[1].Labels[key] != value {
			t.Fatalf("retry lost durable cleanup identity %s", key)
		}
	}
	terminal, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || terminal.FixedCreateIntent == nil || terminal.FixedCreateIntent.State != "released" {
		t.Fatalf("missing terminal receipt after retry: %+v err=%v", terminal, err)
	}
}

func TestFixedAzureExplicitRecoveryCancellationWhileClaimLocked(t *testing.T) {
	client := &fakeAzureClient{}
	b := fixedAzureTestBackend(t, client)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123464", RequestedSlug: "restart-cancel", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.RemoveLeaseClaimIfUnchanged(req.RequestedLeaseID, claim); err != nil {
		t.Fatal(err)
	}
	held, release, lockDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		lockDone <- core.WithDurableLeaseClaimLockContext(t.Context(), req.RequestedLeaseID, func(*core.LeaseClaim, bool, func() error) error {
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case err := <-lockDone:
		t.Fatalf("hold claim fence: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.ReclaimAndStop(ctx, core.StopRequest{ID: req.RequestedLeaseID}) }()
	var stopErr error
	select {
	case stopErr = <-done:
		close(release)
	case <-time.After(2 * time.Second):
		t.Error("recovery ignored its deadline while waiting for the claim fence")
		close(release)
		stopErr = <-done
	}
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("stop error=%v, want deadline exceeded", stopErr)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID); err != nil || exists {
		t.Fatalf("canceled recovery published a claim: exists=%v err=%v", exists, err)
	}
	if len(client.prepareOwned) != 0 || len(client.ownedExpected) != 0 {
		t.Fatal("canceled recovery reached provider cleanup")
	}
}

func TestFixedAzureExplicitRecoveryDiagnosticNamesForceStop(t *testing.T) {
	b := fixedAzureTestBackend(t, &fakeAzureClient{})
	err := b.ReclaimAndStop(t.Context(), core.StopRequest{ID: "restart-recovery"})
	if err == nil || !strings.Contains(err.Error(), "stop --force --provider azure --id") {
		t.Fatalf("diagnostic does not identify the supported command: %v", err)
	}
}

func TestFixedAzureExplicitRecoveryRejectsIncompleteRemoteIdentity(t *testing.T) {
	client := &fakeAzureClient{}
	b := fixedAzureTestBackend(t, client)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123462", RequestedSlug: "restart-reject", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.RemoveLeaseClaimIfUnchanged(req.RequestedLeaseID, claim); err != nil {
		t.Fatal(err)
	}
	delete(client.servers[0].Labels, "fixed_attempt")
	if err := b.ReclaimAndStop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err == nil {
		t.Fatal("incomplete remote identity was adopted")
	}
	if len(client.deleted) != 0 {
		t.Fatalf("deleted=%v", client.deleted)
	}
}
