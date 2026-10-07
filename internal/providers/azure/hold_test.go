package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (c *fakeAzureClient) InspectFailedLeaseHold(_ context.Context, expected core.Server) (core.LeaseRecoveryHold, error) {
	return core.LeaseRecoveryHold{Schema: "crabbox.lease-hold.v1", Provider: "azure", LeaseID: expected.Labels["lease"], Status: "held", UnacceptedChanges: "unknown", Resources: []core.LeaseHeldResource{
		{Kind: "vm", ID: expected.CloudID, ImmutableID: expected.ImmutableID, State: "absent"},
		{Kind: "disk", ID: expected.CloudID + "-osdisk", ImmutableID: "retained-disk", State: "retained"},
	}}, nil
}

func (c *fakeAzureClient) InspectClaimlessFailedLeaseHold(_ context.Context, id, slug string) (core.LeaseRecoveryHold, error) {
	name := core.LeaseProviderName(id, slug)
	return core.LeaseRecoveryHold{Schema: "crabbox.lease-hold.v1", Provider: "azure", LeaseID: id, Status: "held", UnacceptedChanges: "unknown", Resources: []core.LeaseHeldResource{
		{Kind: "vm", ID: name, State: "absent"},
		{Kind: "nic", ID: name + "-nic", State: "absent"},
		{Kind: "public-ip", ID: name + "-pip", State: "absent"},
		{Kind: "disk", ID: name + "-osdisk", ImmutableID: "observed-disk", State: "retained"},
		{Kind: "nsg", ID: name + "-q-nsg", State: "absent"},
	}}, nil
}

func TestAzureClaimlessHoldPersistsObservedOnlyCustody(t *testing.T) {
	client := &fakeAzureClient{}
	b := fixedAzureTestBackend(t, client)
	t.Setenv("CRABBOX_PROVIDER", "azure")
	id, slug := "cbx_abcdef123460", "original-worker"
	var output bytes.Buffer
	app := core.App{Stdout: &output, Stderr: &bytes.Buffer{}}
	if err := app.Run(t.Context(), []string{"hold", "--provider", "azure", "--id", id, "--slug", slug, "--json"}); err != nil {
		t.Fatal(err)
	}
	var receipt core.LeaseRecoveryHold
	if err := json.Unmarshal(output.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(id)
	if err != nil || claim.Provider != "azure-recovery-held-v1" || claim.ProviderScope != client.LeaseClaimScope() ||
		claim.CloudID != core.LeaseProviderName(id, slug) || claim.FixedCreateIntent != nil || claim.CloudImmutableID != "" ||
		claim.RecoveryHold == nil || !reflect.DeepEqual(*claim.RecoveryHold, receipt) {
		t.Fatalf("claimless hold fabricated original binding or was not durable: %+v %v", claim, err)
	}
	if replayed, err := b.HoldFailedLease(t.Context(), id); err != nil || !reflect.DeepEqual(replayed, receipt) {
		t.Fatalf("hold replay changed observed receipt: %+v %v", replayed, err)
	}
	client.claimScope = "subscription:other|resource-group:rg"
	if _, err := b.HoldFailedLease(t.Context(), id); err == nil {
		t.Fatal("held claim replay accepted another account scope")
	}
	client.claimScope = ""
	if _, err := b.HoldFailedLeaseWithSlug(t.Context(), id, "different-worker"); err == nil {
		t.Fatal("hold replay accepted a changed original slug")
	}
	if err := b.ReclaimAndStop(t.Context(), core.StopRequest{ID: id}); err == nil {
		t.Fatal("claimless hold was treated as completed deletion")
	}
	if err := b.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: id, Server: core.Server{CloudID: claim.CloudID, Labels: map[string]string{"lease": id}}}}); err == nil {
		t.Fatal("claimless hold allowed direct release")
	}
	if _, err := b.Acquire(t.Context(), core.AcquireRequest{RequestedLeaseID: id, RequestedSlug: slug, Repo: core.Repo{Root: t.TempDir()}}); err == nil {
		t.Fatal("claimless held ID was reused")
	}
	if err := b.Cleanup(t.Context(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(client.deleted) != 0 || len(client.ownedExpected) != 0 {
		t.Fatal("claimless hold allowed deletion")
	}
}

func TestAzureFailedLeaseHoldCommandPersistsAcrossRestartAndRefusesRelease(t *testing.T) {
	client := &fakeAzureClient{}
	b := fixedAzureTestBackend(t, client)
	t.Setenv("CRABBOX_PROVIDER", "azure")
	id := "cbx_abcdef123459"
	lease, err := b.Acquire(t.Context(), core.AcquireRequest{RequestedLeaseID: id, RequestedSlug: "held", Repo: core.Repo{Root: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	var output, diagnostic bytes.Buffer
	app := core.App{Stdout: &output, Stderr: &diagnostic}
	if err := app.Run(t.Context(), []string{"hold", "--provider", "azure", "--id", id, "--json"}); err != nil {
		t.Fatal(err)
	}
	var receipt core.LeaseRecoveryHold
	if err := json.Unmarshal(output.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	stored, err := core.ReadLeaseClaim(id)
	if err != nil || stored.RecoveryHold == nil || stored.Provider != "azure-recovery-held-v1" || stored.FixedCreateIntent.State != "held" {
		t.Fatalf("hold not durable: %+v %v", stored, err)
	}
	legacy := stored
	legacy.RecoveryHold = nil
	if err := validateExactAzureClaim(legacy, lease.Server, id, client.LeaseClaimScope()); err == nil {
		t.Fatal("a writer ignoring the additive hold accepted its provider marker")
	}
	restarted := NewAzureLeaseBackend(Provider{}.Spec(), b.Cfg, b.RT).(*azureLeaseBackend)
	replayed, err := restarted.HoldFailedLease(t.Context(), id)
	if err != nil || !reflect.DeepEqual(receipt, replayed) {
		t.Fatalf("hold replay differs: %+v %v", replayed, err)
	}
	if err := restarted.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("held lease released")
	}
	if err := restarted.ReclaimAndStop(t.Context(), core.StopRequest{ID: id}); err == nil {
		t.Fatal("force stop released held lease")
	}
	for _, flags := range [][]string{{"stop", "--provider", "azure", "--id", id}, {"stop", "--provider", "azure", "--id", id, "--force"}} {
		if err := app.Run(t.Context(), flags); err == nil {
			t.Fatal("registered stop path released held lease")
		}
	}
	if _, err := restarted.Acquire(t.Context(), core.AcquireRequest{RequestedLeaseID: id, RequestedSlug: "held", Repo: core.Repo{Root: t.TempDir()}}); err == nil {
		t.Fatal("held lease reused")
	}
	if err := restarted.Cleanup(t.Context(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(client.deleted) != 0 {
		t.Fatal("hold allowed Azure deletion")
	}
	after, err := core.ReadLeaseClaim(id)
	if err != nil || !reflect.DeepEqual(stored, after) {
		t.Fatal("held claim changed during refused cleanup")
	}
}
