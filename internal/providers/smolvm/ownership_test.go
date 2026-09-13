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

func TestSmolvmClaimWaitHonorsCallerDeadline(t *testing.T) {
	for _, operation := range []string{"stop", "reuse"} {
		for _, shared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/shared=%t", operation, shared), func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				claim := seedSmolvmClaim(t, "cbx_123456789abc", "blue", "mach_1", t.TempDir(), nil)
				fake := &fakeAPI{machine: ownedTestMachine()}
				gets := 0
				fake.getHook = func(ctx context.Context, _ string) (machineData, error) {
					gets++
					return fake.machine, ctx.Err()
				}
				withFakeAPI(t, fake)
				b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
				entered, release := make(chan struct{}), make(chan struct{})
				holderDone := make(chan error, 1)
				go func() {
					hold := func() error { close(entered); <-release; return nil }
					if shared {
						holderDone <- core.WithLeaseClaimUnchangedShared(t.Context(), claim.LeaseID, claim, hold)
					} else {
						holderDone <- core.WithDurableLeaseClaimLockContext(t.Context(), claim.LeaseID, func(*core.LeaseClaim, bool, func() error) error { return hold() })
					}
				}()
				select {
				case <-entered:
				case err := <-holderDone:
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer cancel()
				repo := t.TempDir()
				done := make(chan error, 1)
				go func() {
					if operation == "stop" {
						done <- b.Stop(ctx, core.StopRequest{ID: claim.LeaseID})
					} else {
						_, err := b.reuseMachine(ctx, fake, claim.LeaseID, repo, true)
						done <- err
					}
				}()
				var err error
				returned := false
				select {
				case err = <-done:
					returned = true
				case <-time.After(2 * time.Second):
				}
				close(release)
				holderErr := <-holderDone
				if !returned {
					err = <-done
				}
				if holderErr != nil {
					t.Fatal(holderErr)
				}
				if !returned || !errors.Is(err, context.DeadlineExceeded) || gets != 0 || len(fake.verbs) != 0 {
					t.Fatalf("claim wait outlived deadline: returned=%t err=%v provider_calls=%d native=%v", returned, err, gets, fake.verbs)
				}
				stored, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID)
				if readErr != nil || !exists || !reflect.DeepEqual(stored, claim) {
					t.Fatalf("canceled wait changed ownership: stored=%+v err=%v", stored, readErr)
				}
				t.Logf("CLAIM_WAIT operation=%s shared=%t returned_before_release=%t deadline=true native_calls=0 claim_unchanged=true", operation, shared, returned)
			})
		}
	}
}

func TestSmolvmCanceledPublicationPreservesSuccessorAndCause(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	repo, successorRepo := core.Repo{Root: t.TempDir()}, t.TempDir()
	entered, release := make(chan struct{}), make(chan struct{})
	holderDone := make(chan error, 1)
	var successor core.LeaseClaim
	fake := &fakeAPI{}
	gets, starts := 0, 0
	fake.getHook = func(ctx context.Context, _ string) (machineData, error) { gets++; return fake.machine, ctx.Err() }
	fake.startHook = func(ctx context.Context, _ string) error { starts++; return ctx.Err() }
	fake.createHook = func(f *fakeAPI) {
		machine := f.machine
		leaseID := machineLeaseID(machine)
		go func() {
			holderDone <- core.WithDurableLeaseClaimLockContext(t.Context(), leaseID, func(claim *core.LeaseClaim, exists bool, persist func() error) error {
				if exists {
					return errors.New("expected absent claim before publication")
				}
				close(entered)
				<-release
				*claim = core.LeaseClaim{LeaseID: leaseID, Slug: "publication-fixture", Provider: providerName,
					ProviderScope: "https://api.smolmachines.com", CloudID: machine.ID, RepoRoot: successorRepo,
					Labels: map[string]string{"provider": providerName, "lease": leaseID, "slug": "publication-fixture",
						"machine_id": machine.ID, "machine_name": machine.Name, machineCreatedLabel: machine.CreatedAt}}
				if err := persist(); err != nil {
					return err
				}
				successor = *claim
				return nil
			})
		}()
		<-entered
	}
	done := make(chan error, 1)
	go func() { _, _, err := b.createMachine(ctx, fake, repo, false, "publication-fixture"); done <- err }()
	select {
	case <-entered:
	case err := <-holderDone:
		t.Fatalf("claim holder failed: %v", err)
	case err := <-done:
		t.Fatalf("creation failed before the claim fence: %v", err)
	}
	cancel()
	close(release)
	holderErr, err := <-holderDone, <-done
	if holderErr != nil {
		t.Fatal(holderErr)
	}
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "claim changed") || gets != 0 || starts != 0 || !reflect.DeepEqual(fake.verbs, []string{"create"}) {
		t.Fatalf("publication lost cancellation or rollback boundary: err=%v gets=%d starts=%d verbs=%v", err, gets, starts, fake.verbs)
	}
	publicCode := 1
	var public core.ExitError
	if core.AsExitError(err, &public) {
		publicCode = public.Code
	}
	if publicCode != 1 {
		t.Fatalf("rollback replaced the canceled acquisition's CLI exit: code=%d err=%v", publicCode, err)
	}
	stored, exists, readErr := core.ReadLeaseClaimWithPresence(successor.LeaseID)
	if readErr != nil || !exists || !reflect.DeepEqual(stored, successor) {
		t.Fatalf("publication changed successor: stored=%+v err=%v", stored, readErr)
	}
	t.Log("PUBLICATION canceled=true create_calls=1 start_calls=0 lookup_calls=0 delete_calls=0 successor_unchanged=true")
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
				if err := b.Stop(context.Background(), core.StopRequest{ID: identifier}); err == nil {
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
			err := b.Stop(ctx, core.StopRequest{ID: "blue"})
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
	result, err := b.Run(context.Background(), core.RunRequest{Repo: core.Repo{Root: t.TempDir()}, NoSync: true, Command: []string{"true"}})
	if err == nil || result.ExitCode != 1 || result.ErrorKind != core.RunErrorProvider {
		t.Fatalf("changed claim cleanup result=%+v err=%v", result, err)
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
	for _, mode := range []string{"start-error", "canceled", "changed-claim", "appeared-claim", "changed-identity", "incomplete-create", "no-repo", "typed-start-and-cleanup"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fake := &fakeAPI{}
			withFakeAPI(t, fake)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			primaryFailure, cleanupFailure := core.Exit(7, "typed start failed"), core.Exit(2, "typed cleanup failed")
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
				if mode == "typed-start-and-cleanup" {
					return primaryFailure
				}
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
				if mode == "typed-start-and-cleanup" {
					return cleanupFailure
				}
				fake.deleted = true
				return nil
			}
			repo := core.Repo{Root: t.TempDir()}
			if mode == "no-repo" {
				repo.Root = ""
			}
			b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
			err := b.Warmup(ctx, core.WarmupRequest{Repo: repo})
			if err == nil {
				t.Fatal("expected setup error")
			}
			wantDelete := mode == "start-error" || mode == "canceled" || mode == "no-repo" || mode == "typed-start-and-cleanup"
			if (fake.deletedID != "") != wantDelete {
				t.Fatalf("deleted=%s err=%v", fake.deletedID, err)
			}
			if !wantDelete && mode != "incomplete-create" && !strings.Contains(err.Error(), "retained") {
				t.Fatalf("missing retained-resource diagnostic: %v", err)
			}
			if mode == "typed-start-and-cleanup" {
				var public core.ExitError
				if !core.AsExitError(err, &public) || public.Code != 7 || !errors.Is(err, primaryFailure) || !errors.Is(err, cleanupFailure) {
					t.Fatalf("typed acquisition outcome lost: public=%+v err=%v", public, err)
				}
				claim, exists, readErr := core.ReadLeaseClaimWithPresence(machineLeaseID(fake.machine))
				if readErr != nil || !exists || claim.CloudID != fake.machine.ID || fake.deleted {
					t.Fatalf("failed rollback lost original ownership: claim=%+v exists=%t err=%v", claim, exists, readErr)
				}
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
			_, err := b.Run(context.Background(), core.RunRequest{ID: "blue", Repo: core.Repo{Root: repo}, Reclaim: reclaim, NoSync: true, Command: []string{"true"}})
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
