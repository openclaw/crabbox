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
