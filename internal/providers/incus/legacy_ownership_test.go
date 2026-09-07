package incus

import (
	"context"
	"maps"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

func claimLegacyFixture(t *testing.T, fake *fakeClient, name, repoRoot string) core.LeaseClaim {
	t.Helper()
	inst := *fake.instances[name]
	labels := labelsFromInstance(inst)
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(labels["lease"], labels["slug"], providerName, instanceScope(name), "", repoRoot, time.Minute, false, serverFromInstance(inst, nil, core.BaseConfig()), core.SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(labels["lease"])
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

// Seed the durable creation record explicitly; production must never upgrade a
// legacy name-only claim from labels on an already existing instance.
func claimDurableFixture(t *testing.T, fake *fakeClient, name string) core.LeaseClaim {
	t.Helper()
	inst := fake.instances[name]
	leaseID := inst.Config[labelKey("lease")]
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		claim = claimLegacyFixture(t, fake, name, t.TempDir())
	}
	identity, err := fake.Identity()
	if err != nil {
		t.Fatal(err)
	}
	uuid := "41927982-c927-499a-9933-6c9a1b0ce268"
	fingerprint := strings.Repeat("a", 64)
	inst.Config["volatile.uuid"] = uuid
	for key, value := range map[string]string{"provider": providerName, "incus_identity": identity.scope(), "incus_uuid": uuid, "fixed_intent_sha256": fingerprint} {
		inst.Config[labelKey(key)] = value
	}
	replacement := claim
	replacement.ProviderScope, replacement.CloudID, replacement.CloudImmutableID = identity.scope(), name, uuid
	replacement.Labels = labelsFromInstance(*inst)
	replacement.FixedCreateIntent = &core.FixedCreateIntent{Version: 1, Fingerprint: fingerprint, ProviderScope: identity.scope(), Slug: claim.Slug, CreatedAt: claim.ClaimedAt, State: "acquired", Attempt: map[string]string{"name": name, "uuid": uuid}}
	if err := core.ReplaceLeaseClaimIfUnchanged(leaseID, claim, replacement); err != nil {
		t.Fatal(err)
	}
	// Keep the claim strictly older than the two-hour-old instance fixtures,
	// even when setup crosses a whole-second timestamp boundary.
	backdateIncusClaim(t, leaseID, time.Now().Add(-24*time.Hour))
	claim, err = core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func legacyOwnershipFixture(t *testing.T, claimed bool) (*backend, *fakeClient, LeaseTarget) {
	t.Helper()
	b, fake, req := lifecycleFixture(t)
	const name = "crabbox-legacy"
	labels := core.DirectLeaseLabels(b.cfg, req.RequestedLeaseID, "legacy", providerName, "", false, time.Now().Add(-2*time.Hour))
	labels["state"], labels["expires_at"] = "expired", core.LeaseLabelTime(time.Now().Add(-time.Hour))
	config := map[string]string{"volatile.uuid": "41927982-c927-499a-9933-6c9a1b0ce268"}
	for key, value := range labels {
		config[labelKey(key)] = value
	}
	fake.instances[name] = &api.Instance{Name: name, Status: "Running", StatusCode: api.Running, InstancePut: api.InstancePut{Config: config}}
	fake.states[name] = &api.InstanceState{Status: "Running", StatusCode: api.Running, Network: map[string]api.InstanceStateNetwork{
		"eth0": {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Address: "192.0.2.10", Scope: "global"}}},
	}}
	server := serverFromInstance(*fake.instances[name], fake.states[name], b.cfg)
	if claimed {
		if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(req.RequestedLeaseID, "legacy", providerName, instanceScope(name), "", req.Repo.Root, time.Minute, false, server, core.SSHTarget{}); err != nil {
			t.Fatal(err)
		}
		backdateIncusClaim(t, req.RequestedLeaseID, time.Now().Add(-2*time.Hour))
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(b.cfg, req.RequestedLeaseID); err != nil {
		t.Fatal(err)
	}
	return b, fake, LeaseTarget{LeaseID: req.RequestedLeaseID, Server: server}
}

func TestLegacyOwnershipReleaseRefusesStopAndDelete(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		for _, remove := range []bool{false, true} {
			t.Run(map[bool]string{false: "claimless", true: "legacy claim"}[claimed]+"/"+map[bool]string{false: "retain", true: "delete"}[remove], func(t *testing.T) {
				b, fake, lease := legacyOwnershipFixture(t, claimed)
				b.cfg.Incus.DeleteOnRelease = remove
				core.MarkDeleteOnReleaseExplicit(&b.cfg, providerName)
				before, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
				if err != nil {
					t.Fatal(err)
				}
				if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, Force: true}); err == nil {
					t.Fatal("legacy release accepted without durable ownership")
				}
				assertLegacyOwnershipUnchanged(t, fake, lease.LeaseID, before, exists)
			})
		}
	}
}

func TestLegacyOwnershipCleanupPreservesInstanceClaimAndKey(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		t.Run(map[bool]string{false: "claimless", true: "legacy claim"}[claimed], func(t *testing.T) {
			b, fake, lease := legacyOwnershipFixture(t, claimed)
			before, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
				t.Fatal(err)
			}
			assertLegacyOwnershipUnchanged(t, fake, lease.LeaseID, before, exists)
		})
	}
}

func TestLegacyOwnershipCannotBeAdoptedByReuse(t *testing.T) {
	b, fake, lease := legacyOwnershipFixture(t, false)
	if _, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.Server.Name, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true}); err == nil {
		t.Fatal("claimless reuse implicitly adopted a legacy instance")
	}
	assertLegacyOwnershipUnchanged(t, fake, lease.LeaseID, core.LeaseClaim{}, false)
}

func TestLegacyOwnershipStatusIsReadOnlyWithRepoRoot(t *testing.T) {
	b, fake, lease := legacyOwnershipFixture(t, false)
	before := maps.Clone(fake.instances[lease.Server.Name].Config)
	if _, err := b.Resolve(context.Background(), ResolveRequest{ID: lease.Server.Name, StatusOnly: true, Repo: core.Repo{Root: t.TempDir()}}); err != nil {
		t.Fatal(err)
	}
	assertLegacyOwnershipUnchanged(t, fake, lease.LeaseID, core.LeaseClaim{}, false)
	if !reflect.DeepEqual(before, fake.instances[lease.Server.Name].Config) {
		t.Fatal("status rewrote provider metadata")
	}
}

func assertLegacyOwnershipUnchanged(t *testing.T, fake *fakeClient, leaseID string, before core.LeaseClaim, existed bool) {
	t.Helper()
	if len(fake.deleted) != 0 || len(fake.stateUpdates) != 0 || len(fake.updated) != 0 {
		t.Fatalf("legacy mutations: deletes=%v states=%v", fake.deleted, fake.stateUpdates)
	}
	after, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || exists != existed || !reflect.DeepEqual(before, after) {
		t.Fatalf("legacy claim changed: existed=%v exists=%v err=%v", existed, exists, err)
	}
	key, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("legacy key lost: %v", err)
	}
}

func TestDurableOwnershipRecheckedAfterStop(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "retain", true: "delete"}[remove], func(t *testing.T) {
			b, fake, req := lifecycleFixture(t)
			lease, err := b.Acquire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			b.cfg.Incus.DeleteOnRelease = remove
			core.MarkDeleteOnReleaseExplicit(&b.cfg, providerName)
			client := &replaceAfterStopClient{fakeClient: fake}
			newClient = func(Config) (instanceClient, error) { return client, nil }
			updatesBefore := len(fake.updated)
			if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, Force: true}); err == nil {
				t.Fatal("replacement after stop was accepted")
			}
			if len(fake.deleted) != 0 || len(fake.updated) != updatesBefore {
				t.Fatal("replacement deleted or relabeled after UUID changed")
			}
			wantState := map[bool]string{false: "acquired", true: "deleting"}[remove]
			claim, err := core.ReadLeaseClaim(lease.LeaseID)
			if err != nil || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.State != wantState {
				t.Fatalf("durable recovery ownership lost: %v", err)
			}
			key, err := core.TestboxKeyPath(lease.LeaseID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(key); err != nil {
				t.Fatalf("recovery key lost: %v", err)
			}
		})
	}
}

func TestDurableOwnershipRejectsConflictingProviderLabel(t *testing.T) {
	for _, operation := range []string{"release", "cleanup"} {
		for _, provider := range []string{"other", ""} {
			t.Run(operation+"/provider="+provider, func(t *testing.T) {
				b, fake, req := lifecycleFixture(t)
				req.Keep = false
				lease, err := b.Acquire(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				inst := fake.instances[lease.Server.Name]
				inst.Config[labelKey("provider")] = provider
				inst.Config[labelKey("expires_at")] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
				backdateIncusClaim(t, lease.LeaseID, time.Now().Add(-2*time.Hour))
				before, err := core.ReadLeaseClaim(lease.LeaseID)
				if err != nil {
					t.Fatal(err)
				}
				fake.updated, fake.stateUpdates = nil, nil
				if operation == "release" {
					if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, Force: true}); err == nil {
						t.Fatal("release accepted conflicting provider label")
					}
				} else if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
					t.Fatal(err)
				}
				assertLegacyOwnershipUnchanged(t, fake, lease.LeaseID, before, true)
			})
		}
	}
}

type replaceAfterStopClient struct{ *fakeClient }

func (c *replaceAfterStopClient) SetInstanceState(name string, put api.InstanceStatePut, etag string) error {
	if err := c.fakeClient.SetInstanceState(name, put, etag); err != nil {
		return err
	}
	if put.Action == "stop" {
		c.instances[name].Config["volatile.uuid"] = "ae04133e-5dc6-4f48-ab2a-ce5ed11e152e"
	}
	return nil
}
