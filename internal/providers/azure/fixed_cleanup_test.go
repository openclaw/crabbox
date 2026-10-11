package azure

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestFixedAzureCleanupRequiresDeletionAdmission(t *testing.T) {
	for _, scenario := range []string{"kept", "unexpired", "expired", "before readiness", "upgraded claim"} {
		t.Run(scenario, func(t *testing.T) {
			client := &fakeAzureClient{}
			backend := fixedAzureTestBackend(t, client)
			request := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123480", RequestedSlug: "retained", Repo: core.Repo{Root: t.TempDir()}, Keep: scenario == "kept"}
			if scenario == "before readiness" {
				client.waitErr = errors.New("access interrupted")
			}
			_, err := backend.Acquire(t.Context(), request)
			if (err != nil) != (scenario == "before readiness") {
				t.Fatal(err)
			}
			if scenario == "upgraded claim" {
				if err := core.WithDurableLeaseClaimLockContext(t.Context(), request.RequestedLeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
					claim.FixedCreateIntent.Journal = nil
					for key := range claim.Labels {
						if strings.HasPrefix(key, "_crabbox_azure_cleanup_") {
							delete(claim.Labels, key)
						}
					}
					return persist()
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := backend.Acquire(t.Context(), request); err != nil {
					t.Fatal(err)
				}
			}
			if err := core.WithDurableLeaseClaimLockContext(t.Context(), request.RequestedLeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
				expiry := time.Now().Add(time.Hour)
				if scenario == "expired" {
					expiry = time.Now().Add(-24 * time.Hour)
				}
				claim.Labels["expires_at"] = core.LeaseLabelTime(expiry)
				return persist()
			}); err != nil {
				t.Fatal(err)
			}
			before, err := core.ReadLeaseClaim(request.RequestedLeaseID)
			if err != nil || !core.HasAzureCleanupBinding(before.Labels) {
				t.Fatalf("missing preparation binding: %v", err)
			}
			client.servers = nil // External VM loss is not permission to delete its disk.
			for _, dryRun := range []bool{true, false} {
				if err := backend.Cleanup(t.Context(), core.CleanupRequest{DryRun: dryRun}); err != nil {
					t.Fatal(err)
				}
			}
			after, err := core.ReadLeaseClaim(request.RequestedLeaseID)
			if err != nil || !reflect.DeepEqual(before, after) || len(client.prepareCleanup) != 0 || len(client.cleanupExpected) != 0 || len(client.deleted) != 0 {
				t.Fatalf("preparation snapshot admitted cleanup: err=%v prepared=%d deletes=%v", err, len(client.prepareCleanup), client.deleted)
			}
		})
	}
}

func TestFixedAzureCleanupPreservesOriginalBinding(t *testing.T) {
	client := &fakeAzureClient{}
	backend := fixedAzureTestBackend(t, client)
	request := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123481", RequestedSlug: "original", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := backend.Acquire(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	client.servers[0].Labels = maps.Clone(client.servers[0].Labels)
	client.servers[0].Labels["state"] = "ready"
	client.servers[0].Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	client.prepareFunc = func(server core.Server) core.Server {
		for key, value := range fixedAzureTestCleanupLabels() {
			if server.Labels[key] != value {
				t.Errorf("automatic cleanup discarded original %s", key)
			}
		}
		return server
	}
	if err := backend.Cleanup(t.Context(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(client.prepareCleanup) != 1 || len(client.cleanupExpected) != 1 {
		t.Fatalf("expected one cleanup: prepared=%d deleted=%d", len(client.prepareCleanup), len(client.cleanupExpected))
	}
}

func TestFixedAzureCleanupJournalsInterruptedDeletion(t *testing.T) {
	client := &fakeAzureClient{}
	backend := fixedAzureTestBackend(t, client)
	request := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123482", RequestedSlug: "interrupted", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := backend.Acquire(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	client.servers[0].Labels = maps.Clone(client.servers[0].Labels)
	client.servers[0].Labels["state"] = "ready"
	client.servers[0].Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	interrupted := errors.New("delete outcome unknown")
	client.deleteCleanupFunc = func(core.Server) error {
		claim, err := core.ReadLeaseClaim(request.RequestedLeaseID)
		if err != nil || claim.FixedCreateIntent.Journal.Phase != "deleting" {
			t.Errorf("deletion was not durably admitted: %v", err)
		}
		return interrupted
	}
	if err := backend.Cleanup(t.Context(), core.CleanupRequest{}); !errors.Is(err, interrupted) {
		t.Fatalf("cleanup error=%v", err)
	}
	// An unresolved DELETE must not permit a still-observable VM to become ready again.
	if _, err := backend.Acquire(t.Context(), request); err == nil {
		t.Error("acquisition replay accepted an unresolved automatic delete")
	}
	client.servers = nil
	client.deleteCleanupFunc = nil
	restarted := NewAzureLeaseBackend(Provider{}.Spec(), backend.Cfg, backend.RT).(*azureLeaseBackend)
	if err := restarted.Cleanup(t.Context(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(request.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.State != "released" || len(client.cleanupExpected) != 2 {
		t.Fatalf("admitted cleanup did not resume: err=%v deletes=%d", err, len(client.cleanupExpected))
	}
}

func TestFixedAzureCleanupResumesLegacyCleanupSnapshot(t *testing.T) {
	client := &fakeAzureClient{}
	backend := fixedAzureTestBackend(t, client)
	request := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123483", RequestedSlug: "legacy-cleanup", Repo: core.Repo{Root: t.TempDir()}}
	if _, err := backend.Acquire(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := core.WithDurableLeaseClaimLockContext(t.Context(), request.RequestedLeaseID, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
		// Pre-journal cleanup writers persisted their snapshot before deletion.
		claim.FixedCreateIntent.Journal = nil
		return persist()
	}); err != nil {
		t.Fatal(err)
	}
	client.servers = nil
	if err := backend.Cleanup(t.Context(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(request.RequestedLeaseID)
	if err != nil || claim.FixedCreateIntent.State != "released" || len(client.cleanupExpected) != 1 {
		t.Fatalf("legacy cleanup did not resume: err=%v deletes=%d", err, len(client.cleanupExpected))
	}
}

func TestFixedAzurePreparationRejectsPreVMReplacement(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "replay"}[replay], func(t *testing.T) {
			client := &fakeAzureClient{}
			backend := fixedAzureTestBackend(t, client)
			request := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123484", RequestedSlug: "pre-vm", Repo: core.Repo{Root: t.TempDir()}}
			if replay {
				client.fixedReplyErr = errors.New("create reply lost")
				if _, err := backend.Acquire(t.Context(), request); err == nil {
					t.Fatal("lost reply unexpectedly succeeded")
				}
				client.fixedReplyErr = nil
			}
			client.prepareFunc = func(server core.Server) core.Server {
				server.Labels = maps.Clone(server.Labels)
				maps.Copy(server.Labels, fixedAzureTestCleanupLabels())
				server.Labels["_crabbox_azure_cleanup_public_ip_id"] = "replacement-ip"
				return server
			}
			if _, err := backend.Acquire(t.Context(), request); err == nil {
				t.Error("prepared a replacement for the original pre-VM public IP")
			}
			claim, err := core.ReadLeaseClaim(request.RequestedLeaseID)
			if err != nil || core.HasAzureCleanupBinding(claim.Labels) || len(client.tagged) != 0 {
				t.Fatalf("replacement published ready or custody: %v", err)
			}
			lease, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: request.RequestedLeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err == nil || len(client.ownedExpected) != 0 {
				t.Error("explicit cleanup adopted a replacement after failed preparation")
			}
			client.servers[0].Labels = maps.Clone(client.servers[0].Labels)
			client.servers[0].Labels["state"] = "ready"
			client.servers[0].Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
			if err := backend.Cleanup(t.Context(), core.CleanupRequest{}); err == nil || len(client.cleanupExpected) != 0 {
				t.Error("automatic cleanup adopted a replacement after failed preparation")
			}
		})
	}
}
