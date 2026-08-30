package smolvm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func seedSmolvmClaim(t *testing.T, id, slug, machineID, repo string, mutate func(*core.LeaseClaim)) core.LeaseClaim {
	t.Helper()
	var result core.LeaseClaim
	err := core.WithDurableLeaseClaimLock(id, func(claim *core.LeaseClaim, _ bool, persist func() error) error {
		*claim = core.LeaseClaim{LeaseID: id, Slug: slug, Provider: "smolvm", CloudID: machineID,
			ProviderScope: "https://api.smolmachines.com", RepoRoot: repo,
			Labels: map[string]string{"provider": "smolvm", "lease": id, "slug": slug, "machine_id": machineID,
				"machine_name": machineName(id, slug), "smolvm_created_at": "2026-08-30T00:00:00Z"}}
		if mutate != nil {
			mutate(claim)
		}
		if err := persist(); err != nil {
			return err
		}
		result = *claim
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func ownedTestMachine() machineData {
	return machineData{ID: "mach_1", Name: "crabbox-blue-123456789abc", State: "running", CreatedAt: "2026-08-30T00:00:00Z"}
}

func TestStopRequiresExactSmolvmClaim(t *testing.T) {
	for _, identifier := range []string{"cbx_123456789abc", "blue", "mach_1", "crabbox-blue-123456789abc"} {
		for _, kind := range []string{"absent", "legacy", "other-provider", "endpoint", "cloud-id", "name", "creation", "duplicate"} {
			t.Run(identifier+"/"+kind, func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				fake := &fakeAPI{machine: ownedTestMachine()}
				withFakeAPI(t, fake)
				if kind != "absent" {
					seedSmolvmClaim(t, "cbx_123456789abc", "blue", "mach_1", t.TempDir(), func(c *core.LeaseClaim) {
						switch kind {
						case "legacy":
							c.CloudID = ""
							c.ProviderScope = ""
							c.Labels = nil
						case "other-provider":
							c.Provider = "other"
						case "endpoint":
							c.ProviderScope = "https://eu.smolmachines.com"
						case "cloud-id":
							c.CloudID = "mach_2"
						case "name":
							c.Labels["machine_name"] = "crabbox-red-123456789abc"
						case "creation":
							c.Labels["smolvm_created_at"] = "yesterday"
						}
					})
				}
				if kind == "duplicate" {
					// Canonical IDs identify one exact claim even if an alias is ambiguous.
					if identifier == "cbx_123456789abc" {
						return
					}
					seedSmolvmClaim(t, "cbx_abcdef123456", "blue", "mach_1", t.TempDir(), func(c *core.LeaseClaim) { c.Labels["machine_name"] = ownedTestMachine().Name })
				}
				before, err := core.ListLeaseClaims()
				if err != nil {
					t.Fatal(err)
				}
				b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
				if err := b.Stop(context.Background(), StopRequest{ID: identifier}); err == nil {
					t.Fatal("unsafe stop succeeded")
				}
				if fake.deletedID != "" {
					t.Fatalf("deleted %s without exact ownership", fake.deletedID)
				}
				after, err := core.ListLeaseClaims()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatal("failed stop changed claims")
				}
			})
		}
	}
}

func TestStopSmolvmConfirmsDeletionBeforeRemovingClaim(t *testing.T) {
	for _, mode := range []string{"success", "initial-404", "text-404", "delete-error", "confirmation-error", "replaced-machine", "cancel-confirmation"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			claim := seedSmolvmClaim(t, "cbx_123456789abc", "blue", "mach_1", t.TempDir(), nil)
			fake := &fakeAPI{machine: ownedTestMachine()}
			withFakeAPI(t, fake)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gets := 0
			fake.getHook = func(context.Context, string) (machineData, error) {
				gets++
				if gets == 1 {
					if mode == "initial-404" {
						return machineData{}, &smolvmAPIError{StatusCode: 404}
					}
					return fake.machine, nil
				}
				// This is an unlocked read, so it can verify the receipt is retained under the operation fence.
				stored, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID)
				if err != nil || !exists || stored.Revision != claim.Revision {
					t.Fatalf("claim removed before confirmation: %v", err)
				}
				switch mode {
				case "text-404":
					return machineData{}, errors.New("upstream 404 not found in diagnostic")
				case "confirmation-error":
					return machineData{}, &smolvmAPIError{StatusCode: 503}
				case "replaced-machine":
					m := fake.machine
					m.CreatedAt = "new-generation"
					return m, nil
				case "cancel-confirmation":
					cancel()
					return fake.machine, nil
				}
				return machineData{}, fmt.Errorf("wrapped: %w", &smolvmAPIError{StatusCode: 404})
			}
			if mode == "delete-error" {
				fake.deleteErr = errors.New("denied")
			}
			b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
			err := b.Stop(ctx, StopRequest{ID: "blue"})
			_, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "success" {
				if err != nil || exists || fake.deletedID != "mach_1" {
					t.Fatalf("err=%v exists=%v deleted=%s", err, exists, fake.deletedID)
				}
			} else if err == nil || !exists {
				t.Fatalf("uncertain deletion lost claim: err=%v exists=%v", err, exists)
			}
		})
	}
}

func TestSmolvmRunRetainsChangedClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{}
	withFakeAPI(t, fake)
	var successor core.LeaseClaim
	fake.streamHook = func() {
		id := machineLeaseID(fake.machine)
		successor = seedSmolvmClaim(t, id, machineSlug(id, fake.machine), "mach_successor", t.TempDir(), nil)
	}
	b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	result, err := b.Run(context.Background(), RunRequest{Repo: Repo{Root: t.TempDir()}, NoSync: true, Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if fake.deletedID != "" || result.Session == nil || !result.Session.Kept {
		t.Fatalf("deleted=%s session=%+v", fake.deletedID, result.Session)
	}
	stored, _, err := core.ReadLeaseClaimWithPresence(successor.LeaseID)
	if err != nil || !reflect.DeepEqual(stored, successor) {
		t.Fatalf("successor changed: %v", err)
	}
}

func TestSmolvmSetupRollbackUsesCreatedIdentity(t *testing.T) {
	for _, mode := range []string{"start-error", "canceled", "changed-claim", "appeared-claim", "changed-identity", "incomplete-create", "no-repo"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := &fakeAPI{}
			withFakeAPI(t, fake)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake.createHook = func(f *fakeAPI) {
				if mode == "incomplete-create" {
					f.machine.CreatedAt = ""
				}
				if mode == "appeared-claim" {
					id := machineLeaseID(f.machine)
					seedSmolvmClaim(t, id, machineSlug(id, f.machine), "mach_successor", t.TempDir(), nil)
				}
			}
			fake.startHook = func(context.Context, string) error {
				if mode == "changed-claim" {
					id := machineLeaseID(fake.machine)
					seedSmolvmClaim(t, id, machineSlug(id, fake.machine), "mach_successor", t.TempDir(), nil)
				}
				if mode == "changed-identity" {
					fake.machine.CreatedAt = "replacement"
				}
				if mode == "canceled" {
					cancel()
				}
				return errors.New("start failed")
			}
			fake.deleteHook = func(cleanupCtx context.Context, id string) error {
				if cleanupCtx.Err() != nil {
					t.Fatalf("cleanup inherited cancellation: %v", cleanupCtx.Err())
				}
				deadline, ok := cleanupCtx.Deadline()
				if !ok || time.Until(deadline) > time.Minute {
					t.Fatal("unbounded cleanup")
				}
				fake.deleted = true
				return nil
			}
			repo := Repo{Root: t.TempDir()}
			if mode == "no-repo" {
				repo.Root = ""
			}
			b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
			err := b.Warmup(ctx, WarmupRequest{Repo: repo})
			if err == nil {
				t.Fatal("expected setup error")
			}
			wantDelete := mode == "start-error" || mode == "canceled" || mode == "no-repo"
			if (fake.deletedID != "") != wantDelete {
				t.Fatalf("deleted=%s err=%v", fake.deletedID, err)
			}
			if !wantDelete && mode != "incomplete-create" && !strings.Contains(err.Error(), "retained") {
				t.Fatalf("missing retained-resource diagnostic: %v", err)
			}
		})
	}
}

func TestSmolvmReuseNeverAdoptsLegacyClaim(t *testing.T) {
	for _, reclaim := range []bool{false, true} {
		t.Run(fmt.Sprint(reclaim), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo := t.TempDir()
			if err := core.ClaimLeaseForRepoProvider("cbx_123456789abc", "blue", providerName, repo, time.Minute, false); err != nil {
				t.Fatal(err)
			}
			before, _, _ := core.ReadLeaseClaimWithPresence("cbx_123456789abc")
			fake := &fakeAPI{machine: ownedTestMachine()}
			withFakeAPI(t, fake)
			b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
			_, err := b.Run(context.Background(), RunRequest{ID: "blue", Repo: Repo{Root: repo}, Reclaim: reclaim, NoSync: true, Command: []string{"true"}})
			if err == nil || len(fake.verbs) != 0 {
				t.Fatalf("err=%v verbs=%v", err, fake.verbs)
			}
			after, _, _ := core.ReadLeaseClaimWithPresence(before.LeaseID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("legacy claim adopted")
			}
		})
	}
}
