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

func (c *fakeAzureClient) CreateFixedServer(ctx context.Context, cfg core.Config, publicKey, leaseID, slug string, labels map[string]string, recordCompanions func(core.AzureFixedCompanions) error) (core.Server, error) {
	if err := recordCompanions(core.AzureFixedCompanions{NICGUID: "original-nic", PublicIPGUID: "original-ip"}); err != nil {
		return core.Server{}, err
	}
	if c.fixedCapacityErr != nil {
		c.createLeaseIDs = append(c.createLeaseIDs, leaseID)
		return core.Server{}, c.fixedCapacityErr
	}
	server, _, err := c.CreateServerWithFallback(ctx, cfg, publicKey, leaseID, slug, true, nil)
	if err == nil {
		server.Labels = maps.Clone(labels)
		c.created = server
		c.servers = append(c.servers, server)
		err = c.fixedReplyErr
	}
	return server, err
}

func (c *fakeAzureClient) SettleRejectedFixedCompanions(_ context.Context, _ core.Server, binding core.AzureFixedCompanions) error {
	c.fixedSettled = append(c.fixedSettled, binding)
	return c.fixedSettleErr
}

func fixedAzureTestBackend(t *testing.T, client *fakeAzureClient) *azureLeaseBackend {
	t.Helper()
	testutil.IsolateUserDirs(t)
	if client.prepareFunc == nil {
		client.prepareFunc = func(server core.Server) core.Server {
			// The real client obtains these from live companion GUIDs/uniqueId.
			if !core.HasAzureCleanupBinding(server.Labels) {
				server.Labels = maps.Clone(server.Labels)
				maps.Copy(server.Labels, fixedAzureTestCleanupLabels())
			}
			return server
		}
	}
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

func fixedAzureTestCleanupLabels() map[string]string {
	return map[string]string{
		core.AzureCleanupBindingLabel:         "v1",
		"_crabbox_azure_cleanup_nic_id":       "original-nic",
		"_crabbox_azure_cleanup_public_ip_id": "original-ip",
		"_crabbox_azure_cleanup_disk_id":      "original-disk",
	}
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

func TestFixedAzureEphemeralReplay(t *testing.T) {
	client := &fakeAzureClient{}
	b := fixedAzureTestBackend(t, client)
	b.Cfg.Azure.OSDisk = core.AzureOSDiskEphemeral
	b.Cfg.ServerType = "Standard_D8ads_v6"
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123456", Repo: core.Repo{Root: t.TempDir()}, Keep: true}
	first, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Server.ImmutableID != replay.Server.ImmutableID || len(client.createLeaseIDs) != 1 {
		t.Fatal("ephemeral replay allocated another VM")
	}
	b.Cfg.Azure.OSDisk = core.AzureOSDiskManaged
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("changed OS disk intent accepted")
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
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || !core.HasAzureCleanupBinding(claim.Labels) || len(client.tagged) != 0 {
		t.Fatalf("access interruption lost capture or published ready: %v", err)
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

func TestFixedAzureRequiredIdentityGatesReadinessAndAdoption(t *testing.T) {
	id := "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/" + strings.Repeat("r", 90) + "/providers/Microsoft.ManagedIdentity/userAssignedIdentities/" + strings.Repeat("i", 128)
	if len(id) <= 256 {
		t.Fatal("fixture does not exceed the Azure tag value limit")
	}
	client := &fakeAzureClient{}
	client.createFunc = func(server core.Server) core.Server {
		server.AzureUserAssignedIdentityIDs = []string{id}
		return server
	}
	b := fixedAzureTestBackend(t, client)
	b.Cfg.Azure.UserAssignedIdentityResourceID = id
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123480", RequestedSlug: "identity", Repo: core.Repo{Root: t.TempDir()}}
	client.waitFunc = func(server core.Server) (core.Server, error) {
		server.AzureUserAssignedIdentityIDs = nil
		return server, nil
	}
	if _, err := b.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "missing required user-assigned identity") {
		t.Fatalf("identity-less readiness error=%v", err)
	}
	if len(client.createLeaseIDs) != 1 || len(client.deleted) != 0 {
		t.Fatal("readiness failure resubmitted or destroyed an uncertain fixed VM")
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.Attempt[fixedAzureUserAssignedIdentityAttempt] != id {
		t.Fatalf("private fixed claim lost the full identity: %+v %v", claim, err)
	}
	for _, value := range client.servers[0].Labels {
		if value == id {
			t.Fatal("full identity resource ID was copied to Azure resource tags")
		}
	}
	client.waitFunc = nil
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if len(client.createLeaseIDs) != 1 {
		t.Fatal("replay created a replacement VM")
	}
	claim, err = core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.Attempt[fixedAzureUserAssignedIdentityAttempt] != id {
		t.Fatalf("replayed claim lost the private identity: %+v %v", claim, err)
	}
	b.Cfg.Azure.UserAssignedIdentityResourceID = id + "-replacement"
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("old ready VM satisfied a new identity requirement")
	}
	if len(client.createLeaseIDs) != 1 {
		t.Fatal("identity change created an in-place replacement")
	}
	b.Cfg.Azure.UserAssignedIdentityResourceID = ""
	if _, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatalf("claim-bound identity was not usable for inspect: %v", err)
	}
	client.servers[0].AzureUserAssignedIdentityIDs = nil
	if _, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID}); err == nil {
		t.Fatal("adopted a VM after its required identity was detached")
	}
	lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatalf("identity loss blocked cleanup: %v", err)
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestFixedAzureExistingUnassignedLeaseCanBeReleasedAfterIdentityOptIn(t *testing.T) {
	client := &fakeAzureClient{}
	b := fixedAzureTestBackend(t, client)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123481", RequestedSlug: "old-claim", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := claim.FixedCreateIntent.Attempt[fixedAzureUserAssignedIdentityAttempt]; exists {
		t.Fatal("unassigned claim does not match the old attempt format")
	}
	b.Cfg.Azure.UserAssignedIdentityResourceID = "/subscriptions/sub/resourceGroups/identities/providers/Microsoft.ManagedIdentity/userAssignedIdentities/worker"
	if _, err := b.Acquire(t.Context(), req); err == nil {
		t.Fatal("existing unassigned VM was reused after enabling identity")
	}
	if len(client.createLeaseIDs) != 1 {
		t.Fatal("identity change allocated a second VM under the old lease ID")
	}
	lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatalf("old claim could not be resolved for release: %v", err)
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
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

func TestFixedAzureCapacityRequiresSettledFirstAttempt(t *testing.T) {
	for _, scenario := range []string{"settled shortage", "companion uncertainty", "noncapacity", "prior submission"} {
		t.Run(scenario, func(t *testing.T) {
			client := &fakeAzureClient{}
			b := fixedAzureTestBackend(t, client)
			req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123477", RequestedSlug: "capacity", Repo: core.Repo{Root: t.TempDir()}}
			shortage := &azureFixedVMShortage{Code: "AllocationFailed", Err: errors.New("structured allocation failure")}
			switch scenario {
			case "settled shortage":
				client.fixedCapacityErr = shortage
			case "companion uncertainty", "prior submission":
				client.fixedCapacityErr, client.fixedSettleErr = shortage, errors.New("companion read uncertain")
			case "noncapacity":
				client.fixedCapacityErr = errors.New("policy rejection")
			}
			_, err := b.Acquire(t.Context(), req)
			var result *core.FixedAllocationResult
			settled := errors.As(err, &result)
			claim, exists, readErr := core.ReadLeaseClaimWithPresence(req.RequestedLeaseID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if scenario == "settled shortage" {
				if !settled || exists || result.Schema != "crabbox.fixed-allocation-result.v1" || result.Capability != "azure-fixed-vm-capacity-v1" ||
					result.Provider != "azure" || result.Category != "capacity_shortage" || result.Allocation != "settled_nonallocation" ||
					result.LeaseID != req.RequestedLeaseID || result.AttemptNonce == "" ||
					result.ProviderCode != "AllocationFailed" || result.Companions != "settled" || len(client.fixedSettled) != 1 {
					t.Fatalf("settled outcome=%+v claim=%+v exists=%v", result, claim, exists)
				}
				return
			}
			if settled || !exists || claim.FixedCreateIntent.Attempt["pre_vm_nic_guid"] == "" {
				t.Fatalf("unsettled claim=%+v result=%+v", claim, result)
			}
			if scenario == "prior submission" {
				client.fixedSettleErr = nil
				_, err = b.Acquire(t.Context(), req)
				if errors.As(err, &result) || len(client.createLeaseIDs) != 1 || len(client.fixedSettled) != 1 {
					t.Fatalf("prior attempt settled or resubmitted: err=%v creates=%d settles=%d", err, len(client.createLeaseIDs), len(client.fixedSettled))
				}
			}
		})
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

func TestFixedAzureMissingClaimAndVMRetainsCompanionCustody(t *testing.T) {
	client := &fakeAzureClient{}
	b := fixedAzureTestBackend(t, client)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123462", RequestedSlug: "lost-worker", Repo: core.Repo{Root: t.TempDir()}}
	_, err := b.Acquire(t.Context(), req)
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
	client.servers = nil // Azure already removed the VM; companions may remain.
	if err := b.ReclaimAndStop(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}); err == nil {
		t.Fatal("missing claim and VM were accepted as full cleanup")
	}
	if len(client.deleted) != 0 || len(client.ownedExpected) != 0 {
		t.Fatalf("claimless recovery mutated Azure: deleted=%v owned=%v", client.deleted, client.ownedExpected)
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

func TestFixedAzureExplicitRecoveryAbsentWithoutBinding(t *testing.T) {
	client := &fakeAzureClient{}
	b := fixedAzureTestBackend(t, client)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123464", RequestedSlug: "absent", Repo: core.Repo{Root: t.TempDir()}}
	_, err := b.Acquire(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.WithDurableLeaseClaimLockContext(t.Context(), req.RequestedLeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
		// Represent an installed legacy claim created before preparation capture.
		for key := range claim.Labels {
			if strings.HasPrefix(key, "_crabbox_azure_cleanup_") {
				delete(claim.Labels, key)
			}
		}
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	client.prepareFunc = nil
	client.servers = nil // Legacy external cleanup removed the VM without a binding.
	t.Chdir(req.Repo.Root)
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "azure")
	t.Setenv("CRABBOX_COORDINATOR", "")
	app := core.App{Stdout: io.Discard, Stderr: io.Discard}
	args := []string{"stop", "--force", "--provider", "azure", "--id", req.RequestedLeaseID}
	interrupted := errors.New("absence verification interrupted")
	client.deleteOwnedFunc = func(server core.Server) error {
		stored, err := core.ReadLeaseClaim(req.RequestedLeaseID)
		if err != nil || stored.FixedCreateIntent.State == "released" || stored.Labels[core.AzureCleanupBindingLabel] != "" || server.Labels[core.AzureCleanupBindingLabel] != "" {
			t.Fatalf("recovery changed binding or published terminal state before verification: %v", err)
		}
		return interrupted
	}
	if err := app.Run(t.Context(), args); !errors.Is(err, interrupted) {
		t.Fatalf("first recovery: %v", err)
	}
	retained, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || retained.FixedCreateIntent.State == "released" || retained.Labels[core.AzureCleanupBindingLabel] != "" {
		t.Fatalf("interruption lost unchanged-format claim: %v", err)
	}
	client.deleteOwnedFunc = nil
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	terminal, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || terminal.FixedCreateIntent.State != "released" || terminal.Labels[core.AzureCleanupBindingLabel] != "" {
		t.Fatalf("missing existing-format terminal receipt: %v", err)
	}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("terminal retry: %v", err)
	}
	if len(client.ownedExpected) != 2 {
		t.Fatalf("terminal retry repeated provider work: %d", len(client.ownedExpected))
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
	client.prepareOwned = nil // Observe only the canceled recovery.
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

func TestFixedAzurePreparationPersistsCleanupBeforeReady(t *testing.T) {
	client := &fakeAzureClient{tagErr: errors.New("ready reply lost")}
	b := fixedAzureTestBackend(t, client)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123470", RequestedSlug: "prepared", Repo: core.Repo{Root: t.TempDir()}}
	acquired := 0
	req.OnAcquired = func(core.LeaseTarget) error { acquired++; return nil }
	client.setTagsFunc = func() {
		claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
		if err != nil || claim.FixedCreateIntent.Journal.Phase != "bound" {
			t.Fatalf("cleanup was not durably bound before ready: %v", err)
		}
		for key, value := range fixedAzureTestCleanupLabels() {
			if claim.Labels[key] != value || client.taggedLabels[len(client.taggedLabels)-1][key] != "" {
				t.Fatalf("identity %s missing from claim or published as cloud tag", key)
			}
		}
	}
	if _, err := b.Acquire(t.Context(), req); !errors.Is(err, client.tagErr) || acquired != 0 {
		t.Fatalf("failed ready publication: err=%v acquired=%d", err, acquired)
	}
	client.tagErr = nil
	lease, err := b.Acquire(t.Context(), req)
	if err != nil || acquired != 1 || len(client.createLeaseIDs) != 1 {
		t.Fatalf("prepared replay: err=%v acquired=%d creates=%d", err, acquired, len(client.createLeaseIDs))
	}
	for key, value := range fixedAzureTestCleanupLabels() {
		if lease.Server.Labels[key] != value || client.prepareOwned[1].Labels[key] != value {
			t.Fatalf("replay did not preserve original %s", key)
		}
	}
	client.servers = nil // Guest/provider removed only the VM, after genuine capture.
	missing, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	client.prepareFunc = nil // Release must use the retained original binding.
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: missing}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.State != "released" || len(client.ownedExpected) != 1 {
		t.Fatalf("missing finalization: err=%v deletes=%d", err, len(client.ownedExpected))
	}
	for key, value := range fixedAzureTestCleanupLabels() {
		if client.ownedExpected[0].Labels[key] != value {
			t.Fatalf("release lost %s", key)
		}
	}
}

func TestFixedAzureEndpointRefreshPreservesEvictedCleanup(t *testing.T) {
	for _, test := range []struct {
		name        string
		failCleanup bool
		replaceVM   bool
	}{
		{name: "settled"},
		{name: "unknown cleanup", failCleanup: true},
		{name: "replacement endpoint", replaceVM: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeAzureClient{}
			backend := fixedAzureTestBackend(t, client)
			request := core.AcquireRequest{
				RequestedLeaseID: "cbx_abcdef123471", RequestedSlug: "evicted",
				Repo: core.Repo{Root: t.TempDir()}, Keep: true,
			}
			lease, err := backend.Acquire(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			claim, err := core.ReadLeaseClaim(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			// A normal cloud endpoint read contains public tags, not the local
			// companion binding captured before the VM became ready.
			observed := client.servers[0]
			observed.Labels = maps.Clone(observed.Labels)
			observed.Labels["state"] = "ready"
			if test.replaceVM {
				observed.ImmutableID = "replacement-vm"
			}
			refreshed, err := core.UpdateLeaseClaimEndpointIfUnchanged(
				lease.LeaseID, claim, observed, lease.SSH,
			)
			if test.replaceVM {
				retained, readErr := core.ReadLeaseClaim(lease.LeaseID)
				if err == nil || readErr != nil || !reflect.DeepEqual(retained, claim) || len(client.deleted) != 0 {
					t.Fatalf("replacement changed ownership: err=%v readErr=%v deleted=%v", err, readErr, client.deleted)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for key, value := range fixedAzureTestCleanupLabels() {
				if refreshed.Labels[key] != value {
					t.Fatalf("endpoint refresh lost recorded companion %s", key)
				}
			}
			client.servers = nil     // Spot removal leaves the originally bound companions.
			client.prepareFunc = nil // Recovery must not recapture replacement identities.
			uncertain := errors.New("companion deletion outcome unknown")
			client.deleteOwnedFunc = func(server core.Server) error {
				for key, value := range fixedAzureTestCleanupLabels() {
					if server.Labels[key] != value {
						t.Fatalf("eviction cleanup did not use original %s", key)
					}
				}
				if test.failCleanup {
					return uncertain
				}
				return nil
			}
			resolved, err := backend.Resolve(t.Context(), core.ResolveRequest{
				ID: lease.LeaseID, ReleaseOnly: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			err = backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: resolved})
			retained, readErr := core.ReadLeaseClaim(lease.LeaseID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if test.failCleanup {
				if !errors.Is(err, uncertain) || retained.FixedCreateIntent.State == "released" || len(client.deleted) != 0 {
					t.Fatalf("uncertain deletion released custody: err=%v claim=%+v", err, retained)
				}
				for key, value := range fixedAzureTestCleanupLabels() {
					if retained.Labels[key] != value {
						t.Fatalf("uncertain cleanup lost %s", key)
					}
				}
				return
			}
			if err != nil || retained.FixedCreateIntent.State != "released" || len(client.deleted) != 1 {
				t.Fatalf("evicted cleanup did not settle: err=%v claim=%+v deleted=%v", err, retained, client.deleted)
			}
		})
	}
}

func TestFixedAzurePreparationFailureRetainsUnreadyClaim(t *testing.T) {
	for _, failure := range []string{"read", "binding"} {
		t.Run(failure, func(t *testing.T) {
			client := &fakeAzureClient{}
			b := fixedAzureTestBackend(t, client)
			if failure == "read" {
				client.prepareErr = errors.New("companion identity unavailable")
			} else {
				client.prepareFunc = func(server core.Server) core.Server { return server }
			}
			req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123471", RequestedSlug: "unready", Repo: core.Repo{Root: t.TempDir()}}
			acquired := false
			req.OnAcquired = func(core.LeaseTarget) error { acquired = true; return nil }
			if _, err := b.Acquire(t.Context(), req); err == nil {
				t.Fatal("capture failure accepted")
			}
			claim, err := core.ReadLeaseClaim(req.RequestedLeaseID)
			if err != nil || claim.CloudImmutableID == "" || core.HasAzureCleanupBinding(claim.Labels) || acquired || len(client.tagged) != 0 || len(client.deleted) != 0 {
				t.Fatalf("unknown capture published ready/deleted/lost claim: %v", err)
			}
		})
	}
}

func TestFixedAzureReleaseUsesClaimBindingWithLiveVM(t *testing.T) {
	client := &fakeAzureClient{}
	b := fixedAzureTestBackend(t, client)
	req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123472", RequestedSlug: "projection", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := b.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	lease, err := b.Resolve(t.Context(), core.ResolveRequest{ID: req.RequestedLeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if core.HasAzureCleanupBinding(lease.Server.Labels) {
		t.Fatal("live projection unexpectedly contains private binding")
	}
	client.prepareFunc = func(server core.Server) core.Server {
		for key, value := range fixedAzureTestCleanupLabels() {
			if server.Labels[key] != value {
				t.Fatalf("release recaptured instead of checking original %s", key)
			}
		}
		return server
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}
