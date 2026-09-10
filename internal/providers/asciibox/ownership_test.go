package asciibox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func ownedFixture(t *testing.T) (*backend, *fakeAPI, LeaseClaim, LeaseTarget) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("BOX_ORG", "")
	f := &fakeAPI{box: testBox()}
	withFakeAPI(t, f)
	stubSSHWait(t)
	b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	claim, err := publishBoxClaim(testConfig(), "cbx_123456789abc", "proof", t.TempDir(), f.box, true)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return b, f, claim, lease
}

func assertClaimRetained(t *testing.T, claim LeaseClaim) {
	t.Helper()
	got, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if err != nil || !exists || !reflect.DeepEqual(got, claim) {
		t.Fatalf("claim changed: exists=%t err=%v", exists, err)
	}
}

func assertCompletedDeletionRetained(t *testing.T, original LeaseClaim) LeaseClaim {
	t.Helper()
	claim, exists, err := core.ReadLeaseClaimWithPresence(original.LeaseID)
	if err != nil || !exists || !boxFromClaim(claim).deletionCompleted {
		t.Fatalf("completed deletion not retained: exists=%t err=%v", exists, err)
	}
	withoutCompletion := claim
	withoutCompletion.Revision = original.Revision
	withoutCompletion.LastUsedAt = original.LastUsedAt
	withoutCompletion.Labels = make(map[string]string, len(claim.Labels))
	for key, value := range claim.Labels {
		if key != boxDeletionLabel {
			withoutCompletion.Labels[key] = value
		}
	}
	if !reflect.DeepEqual(withoutCompletion, original) {
		t.Fatal("recording completion changed unrelated claim content")
	}
	return claim
}

func assertPendingDeletionRetained(t *testing.T, original LeaseClaim, operationID string) LeaseClaim {
	t.Helper()
	claim, exists, err := core.ReadLeaseClaimWithPresence(original.LeaseID)
	if err != nil || !exists || claim.Labels[boxDeletionOperationLabel] != operationID || claim.Labels[boxDeletionOperationBindingLabel] != boxDeletionOperationBinding(claim, operationID) || claim.Labels[boxDeletionLabel] != "" {
		t.Fatalf("pending reference was not retained without completion: exists=%t err=%v", exists, err)
	}
	withoutReference := claim
	withoutReference.Revision = original.Revision
	withoutReference.LastUsedAt = original.LastUsedAt
	withoutReference.Labels = make(map[string]string, len(claim.Labels))
	for key, value := range claim.Labels {
		if key != boxDeletionOperationLabel && key != boxDeletionOperationBindingLabel {
			withoutReference.Labels[key] = value
		}
	}
	if !reflect.DeepEqual(withoutReference, original) {
		t.Fatal("recording accepted operation changed unrelated claim content")
	}
	return claim
}

func completedDeletionFixture(t *testing.T) (*backend, *fakeAPI, LeaseClaim) {
	t.Helper()
	b, f, claim, lease := ownedFixture(t)
	inventoryErr := errors.New("complete inventory unavailable")
	f.listHook = func() ([]boxData, error) { return nil, inventoryErr }
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); !errors.Is(err, inventoryErr) {
		t.Fatalf("release err=%v, want inventory failure", err)
	}
	completed := assertCompletedDeletionRetained(t, claim)
	f.listHook = nil
	return b, f, completed
}

func pendingDeletionFixture(t *testing.T) (*backend, *fakeAPI, LeaseClaim) {
	t.Helper()
	b, f, claim, lease := ownedFixture(t)
	f.releaseHook = func(id string) error {
		f.deleted = true
		return &boxDeletionIncompleteError{operation: boxDeletionOperation{ID: testDeletionID, Kind: "box", TargetID: id, Status: "pending"}, err: context.DeadlineExceeded}
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("release err=%v, want deadline", err)
	}
	pending := assertPendingDeletionRetained(t, claim, testDeletionID)
	f.releaseHook = nil
	return b, f, pending
}

func TestReleaseAcceptedNativeDeletionDoesNotRecordCompletion(t *testing.T) {
	for _, test := range []struct {
		name     string
		cancelOn string
		poll     commandOutcome
	}{
		{name: "blocked then lookup failure", poll: deletionOutcome(testDeletionID, "bx_1", "box", "blocked")},
		{name: "canceled acceptance", cancelOn: "delete"},
		{name: "canceled poll", cancelOn: "deletion", poll: deletionOutcome(testDeletionID, "bx_1", "box", "blocked")},
		{name: "changed poll target", poll: deletionOutcome(testDeletionID, "bx_other", "box", "completed")},
		{name: "malformed poll", poll: commandOutcome{result: LocalCommandResult{Stdout: `{"operation":null}`}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, claim, _ := ownedFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			info := commandOutcome{result: LocalCommandResult{Stdout: fmt.Sprintf(`{"box":{"id":%q,"createdAt":%q}}`, claim.CloudID, claim.Labels[boxCreationLabel])}}
			runner := &releaseCommandRunner{configPath: t.TempDir() + "/config.json", outcomes: map[string][]commandOutcome{
				"info": {info, info, info, info}, "stop": {{result: LocalCommandResult{}}},
				"delete":   {deletionOutcome(testDeletionID, claim.CloudID, "box", "pending")},
				"deletion": {test.poll},
				"list":     {{result: LocalCommandResult{Stderr: "confirmation unavailable"}, err: errors.New("exit status 1")}},
			}, onAction: func(action string) {
				if action == test.cancelOn {
					cancel()
				}
			}}
			c := &client{apiKey: "box_test", apiURL: "https://ascii.dev", home: t.TempDir(), cliPath: "box", runner: runner, releasePollInterval: time.Nanosecond}
			if err := releaseClaimedBox(ctx, c, claim, nil); err == nil {
				t.Fatal("accepted native deletion was treated as completed")
			}
			assertPendingDeletionRetained(t, claim, testDeletionID)
		})
	}
}

func TestReleaseCompletedNativeDeletionRetainsWitnessUntilConfirmation(t *testing.T) {
	_, _, claim, _ := ownedFixture(t)
	info := commandOutcome{result: LocalCommandResult{Stdout: fmt.Sprintf(`{"box":{"id":%q,"createdAt":%q}}`, claim.CloudID, claim.Labels[boxCreationLabel])}}
	runner := &releaseCommandRunner{configPath: t.TempDir() + "/config.json", outcomes: map[string][]commandOutcome{
		"info": {info, info, info, info}, "stop": {{result: LocalCommandResult{}}},
		"delete":   {deletionOutcome(testDeletionID, claim.CloudID, "box", "pending")},
		"deletion": {deletionOutcome(testDeletionID, claim.CloudID, "box", "blocked"), deletionOutcome(testDeletionID, claim.CloudID, "box", "completed")},
		"list":     {{result: LocalCommandResult{Stderr: "confirmation unavailable"}, err: errors.New("exit status 1")}},
	}}
	c := &client{apiKey: "box_test", apiURL: "https://ascii.dev", home: t.TempDir(), cliPath: "box", runner: runner, releasePollInterval: time.Nanosecond}
	if err := releaseClaimedBox(context.Background(), c, claim, nil); err == nil || !strings.Contains(err.Error(), "confirmation unavailable") {
		t.Fatalf("release err=%v, want inventory confirmation failure", err)
	}
	completed := assertCompletedDeletionRetained(t, claim)
	runner.outcomes["info"] = []commandOutcome{{result: LocalCommandResult{Stderr: "box not found (404)"}, err: errors.New("exit status 1")}}
	runner.outcomes["list"] = []commandOutcome{{result: LocalCommandResult{Stdout: `{"boxes":[]}`}}}
	commandCount := len(runner.commands)
	if err := releaseClaimedBox(context.Background(), c, completed, nil); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("claim remains after completed deletion confirmation: exists=%t err=%v", exists, err)
	}
	for _, command := range runner.commands[commandCount:] {
		if strings.Contains(command, " delete ") || strings.Contains(command, " stop ") || strings.Contains(command, " extend ") {
			t.Fatalf("retry repeated mutation: %s", command)
		}
	}
}

func TestReleaseMalformedAcceptanceDoesNotRecordOperation(t *testing.T) {
	for _, response := range []string{`{}`, `{"operation":null}`, deletionOutcome(testDeletionID, "bx_other", "box", "pending").result.Stdout} {
		t.Run(response, func(t *testing.T) {
			_, _, claim, _ := ownedFixture(t)
			info := commandOutcome{result: LocalCommandResult{Stdout: fmt.Sprintf(`{"box":{"id":%q,"createdAt":%q}}`, claim.CloudID, claim.Labels[boxCreationLabel])}}
			runner := &releaseCommandRunner{configPath: t.TempDir() + "/config.json", outcomes: map[string][]commandOutcome{
				"info": {info, info, info, info}, "stop": {{result: LocalCommandResult{}}}, "delete": {{result: LocalCommandResult{Stdout: response}}},
			}}
			c := &client{apiKey: "box_test", apiURL: "https://ascii.dev", home: t.TempDir(), cliPath: "box", runner: runner}
			if err := releaseClaimedBox(context.Background(), c, claim, nil); err == nil {
				t.Fatal("malformed acceptance succeeded")
			}
			assertClaimRetained(t, claim)
		})
	}
}

func TestReleasePendingReferenceRejectsChangedBinding(t *testing.T) {
	for _, field := range []string{boxDeletionOperationLabel, boxDeletionOperationBindingLabel, "owner", "generation", "missing reference", "missing binding"} {
		t.Run(field, func(t *testing.T) {
			b, f, claim := pendingDeletionFixture(t)
			changed := claim
			changed.Labels = make(map[string]string, len(claim.Labels))
			for key, value := range claim.Labels {
				changed.Labels[key] = value
			}
			switch field {
			case boxDeletionOperationLabel:
				changed.Labels[field] = "bdop_fedcba9876543210fedcba9876543210"
			case boxDeletionOperationBindingLabel:
				changed.Labels[field] = "unverified"
			case "owner":
				changed.RepoRoot = t.TempDir()
			case "generation":
				changed.Labels[boxCreationLabel] = "2026-08-30T12:00:01Z"
			case "missing reference":
				delete(changed.Labels, boxDeletionOperationLabel)
			case "missing binding":
				delete(changed.Labels, boxDeletionOperationBindingLabel)
			}
			changed, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, changed)
			if err != nil {
				t.Fatal(err)
			}
			f.deletionHook = func(string, string) (boxDeletionOperation, error) {
				t.Fatal("invalid reference reached provider lookup")
				return boxDeletionOperation{}, nil
			}
			if _, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true}); err == nil {
				t.Fatal("changed pending binding authorized lookup")
			}
			assertClaimRetained(t, changed)
		})
	}
}

func TestReleasePendingReferenceRechecksCompletionInsideFence(t *testing.T) {
	b, f, claim := pendingDeletionFixture(t)
	reads := 0
	f.deletionHook = func(targetID, operationID string) (boxDeletionOperation, error) {
		reads++
		op := boxDeletionOperation{ID: operationID, Kind: "box", TargetID: targetID, Status: "completed", CompletedAt: "2026-09-02T09:00:00Z"}
		if reads > 1 {
			op.Status = "blocked"
			op.CompletedAt = ""
		}
		return op, nil
	}
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	teardown := false
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(context.Context, LeaseTarget) { teardown = true }}); err == nil {
		t.Fatal("earlier completion read replaced release-fence verification")
	}
	if reads != 2 || teardown || len(f.deletedIDs) != 0 || len(f.prepareIDs) != 0 {
		t.Fatalf("unsafe pending retry: reads=%d teardown=%t deleted=%v prepared=%v", reads, teardown, f.deletedIDs, f.prepareIDs)
	}
	assertClaimRetained(t, claim)
}

func TestReleasePendingReferenceRejectsUncertainOperation(t *testing.T) {
	for _, failure := range []string{"operation", "target", "kind", "completion timestamp", "lookup", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			b, f, claim := pendingDeletionFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.deletionHook = func(targetID, operationID string) (boxDeletionOperation, error) {
				op := boxDeletionOperation{ID: operationID, TargetID: targetID, Kind: "box", Status: "completed", CompletedAt: "2026-09-02T09:00:00Z"}
				switch failure {
				case "operation":
					op.ID = "bdop_fedcba9876543210fedcba9876543210"
				case "target":
					op.TargetID = "bx_replacement"
				case "kind":
					op.Kind = "account"
				case "completion timestamp":
					op.CompletedAt = ""
				case "lookup":
					return boxDeletionOperation{}, errors.New("operation not found (404)")
				case "canceled":
					cancel()
				}
				return op, nil
			}
			f.getHook = func(string) (boxData, error) {
				t.Fatal("uncertain operation reached Box lookup")
				return boxData{}, nil
			}
			if _, err := b.Resolve(ctx, ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true}); err == nil {
				t.Fatal("uncertain operation authorized cleanup")
			}
			assertClaimRetained(t, claim)
		})
	}
}

func TestReleasePendingDeletionSurvivesTimeoutAndRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b, _, claim, _ := ownedFixture(t)
		info := commandOutcome{result: LocalCommandResult{Stdout: fmt.Sprintf(`{"box":{"id":%q,"createdAt":%q}}`, claim.CloudID, claim.Labels[boxCreationLabel])}}
		runner := &releaseCommandRunner{configPath: t.TempDir() + "/config.json", outcomes: map[string][]commandOutcome{
			"info": {info, info, info, info}, "stop": {{result: LocalCommandResult{}}},
			"delete": {deletionOutcome(testDeletionID, claim.CloudID, "box", "pending")},
		}}
		c := &client{apiKey: "box_test", apiURL: "https://ascii.dev", home: t.TempDir(), cliPath: "box", runner: runner, releasePollInterval: time.Hour}
		withFakeAPI(t, c)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if err := releaseClaimedBox(ctx, c, claim, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("release err=%v, want deadline", err)
		}
		pending := assertPendingDeletionRetained(t, claim, testDeletionID)
		runner.outcomes["deletion"] = []commandOutcome{deletionOutcome(testDeletionID, claim.CloudID, "box", "blocked")}
		commandCount := len(runner.commands)
		if _, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true}); err == nil {
			t.Fatal("blocked operation was treated as completed")
		}
		assertClaimRetained(t, pending)
		for _, command := range runner.commands[commandCount:] {
			if strings.Contains(command, " info ") || strings.Contains(command, " list ") || strings.Contains(command, " stop ") || strings.Contains(command, " delete ") || strings.Contains(command, " ssh ") {
				t.Fatalf("pending retry used Box lookup or mutation: %s", command)
			}
		}
		runner.outcomes["deletion"] = []commandOutcome{deletionOutcome(testDeletionID, claim.CloudID, "box", "completed"), deletionOutcome(testDeletionID, claim.CloudID, "box", "completed")}
		notFound := commandOutcome{result: LocalCommandResult{Stderr: "box not found (404)"}, err: errors.New("exit status 1")}
		empty := commandOutcome{result: LocalCommandResult{Stdout: `{"boxes":[]}`}}
		runner.outcomes["info"] = []commandOutcome{notFound, notFound}
		runner.outcomes["list"] = []commandOutcome{empty, empty}
		lease, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
			t.Fatal(err)
		}
		if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
			t.Fatalf("completed operation did not finalize: exists=%t err=%v", exists, err)
		}
	})
}

func TestReleaseRejectsUnownedAndChangedClaims(t *testing.T) {
	for _, name := range []string{"missing snapshot", "changed revision", "removed claim", "wrong lease", "wrong cloud ID", "wrong box label", "wrong provider", "wrong scope", "missing creation", "changed slug", "changed owner", "changed organization"} {
		t.Run(name, func(t *testing.T) {
			b, f, claim, lease := ownedFixture(t)
			switch name {
			case "missing snapshot":
				lease.Server = boxToServer(testConfig(), f.box, claim.LeaseID, claim.Slug, true)
			case "wrong lease":
				lease.LeaseID = "cbx_abcdefabcdef"
			case "wrong cloud ID":
				lease.Server.CloudID = "bx_other"
			case "wrong box label":
				lease.Server.Labels["box_id"] = "bx_other"
			case "changed organization":
				t.Setenv("BOX_ORG", "other")
			case "removed claim":
				if err := core.RemoveLeaseClaimIfUnchangedAfter(claim.LeaseID, claim, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
			default:
				changed := claim
				changed.Labels = map[string]string{}
				for k, v := range claim.Labels {
					changed.Labels[k] = v
				}
				switch name {
				case "changed revision":
					changed.LastUsedAt = "2026-08-30T13:00:00Z"
				case "wrong provider":
					changed.Provider = "other"
				case "wrong scope":
					changed.ProviderScope = "box:bx_1"
				case "missing creation":
					delete(changed.Labels, boxCreationLabel)
				case "changed slug":
					changed.Slug = "new-owner"
				case "changed owner":
					changed.RepoRoot = t.TempDir()
				}
				var err error
				claim, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, changed)
				if err != nil {
					t.Fatal(err)
				}
			}
			teardown := false
			err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(context.Context, LeaseTarget) { teardown = true }})
			if err == nil || len(f.deletedIDs) != 0 || teardown {
				t.Fatalf("unsafe release: err=%v deleted=%v teardown=%t", err, f.deletedIDs, teardown)
			}
			if name != "removed claim" {
				assertClaimRetained(t, claim)
			}
		})
	}
}

func TestReleaseRetainsUncertainResources(t *testing.T) {
	for _, name := range []string{"wrong ID", "missing timestamp", "changed timestamp", "lookup 404", "lookup failure", "failed deletion", "bad confirmation", "cancelled", "replacement in inventory"} {
		t.Run(name, func(t *testing.T) {
			b, f, claim, lease := ownedFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantDelete := false
			switch name {
			case "wrong ID":
				wrong := f.box
				wrong.ID = "bx_other"
				f.getHook = func(string) (boxData, error) { return wrong, nil }
			case "missing timestamp":
				f.box.CreatedAt = nil
			case "changed timestamp":
				f.box.CreatedAt = "2026-08-30T12:00:01Z"
			case "lookup 404":
				f.getHook = func(string) (boxData, error) { return boxData{}, fmt.Errorf("404 not found") }
			case "lookup failure":
				f.getHook = func(string) (boxData, error) { return boxData{}, fmt.Errorf("network unavailable") }
			case "failed deletion":
				f.releaseHook = func(string) error { return fmt.Errorf("delete pending") }
				f.listHook = func() ([]boxData, error) { return []boxData{}, nil }
			case "bad confirmation":
				f.listHook = func() ([]boxData, error) { return nil, fmt.Errorf("partial inventory") }
				wantDelete = true
			case "replacement in inventory":
				replacement := f.box
				replacement.CreatedAt = "2026-08-30T12:00:01Z"
				f.listHook = func() ([]boxData, error) { return []boxData{replacement}, nil }
				wantDelete = true
			case "cancelled":
				cancel()
			}
			err := b.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease})
			if err == nil || (len(f.deletedIDs) > 0) != wantDelete {
				t.Fatalf("release err=%v deleted=%v", err, f.deletedIDs)
			}
			if wantDelete {
				assertCompletedDeletionRetained(t, claim)
			} else {
				assertClaimRetained(t, claim)
			}
		})
	}
}

func TestReleaseRetainsAbsentBoxWithoutCompletedDeletion(t *testing.T) {
	b, f, claim, lease := ownedFixture(t)
	f.getHook = func(string) (boxData, error) { return boxData{}, fmt.Errorf("404 not found") }
	f.listHook = func() ([]boxData, error) { return []boxData{}, nil }

	if _, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true}); err == nil {
		t.Fatal("release-only resolution accepted absence without deletion completion")
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("release accepted absence without deletion completion")
	}
	if len(f.deletedIDs) != 0 {
		t.Fatalf("deleted=%v, want no unverified native deletion", f.deletedIDs)
	}
	assertClaimRetained(t, claim)
}

func TestReleaseRejectsCancellationDuringAbsenceCheck(t *testing.T) {
	b, f, claim := completedDeletionFixture(t)
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.getHook = func(string) (boxData, error) { return boxData{}, fmt.Errorf("404 not found") }
	f.listHook = func() ([]boxData, error) {
		cancel()
		return []boxData{}, nil
	}
	if err := b.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease}); !errors.Is(err, context.Canceled) {
		t.Fatalf("release err=%v, want cancellation", err)
	}
	assertClaimRetained(t, claim)
}

func TestReleaseRetriesCompletedDeletionAfterConfirmationFailure(t *testing.T) {
	b, f, claim := completedDeletionFixture(t)
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	teardown := false
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(context.Context, LeaseTarget) {
		teardown = true
	}}); err != nil {
		t.Fatal(err)
	}
	if teardown || len(f.deletedIDs) != 1 {
		t.Fatalf("retry repeated native cleanup: teardown=%t deleted=%v", teardown, f.deletedIDs)
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("finalized claim exists=%t err=%v", exists, err)
	}
}

func TestReleaseRecordsCompletionWhenConfirmationCanceled(t *testing.T) {
	b, f, claim, lease := ownedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.listHook = func() ([]boxData, error) {
		cancel()
		return []boxData{}, nil
	}
	if err := b.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: lease}); !errors.Is(err, context.Canceled) {
		t.Fatalf("release err=%v, want canceled confirmation", err)
	}
	assertCompletedDeletionRetained(t, claim)
	f.listHook = nil
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if len(f.deletedIDs) != 1 {
		t.Fatalf("retry duplicated native deletion: %v", f.deletedIDs)
	}
}

func TestReleaseRejectsChangedCompletionWitness(t *testing.T) {
	for _, name := range []string{"missing", "corrupt", "legacy digest", "owner", "resource", "creation", "scope", "revision"} {
		t.Run(name, func(t *testing.T) {
			b, f, claim := completedDeletionFixture(t)
			lease, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			changed := claim
			changed.Labels = make(map[string]string, len(claim.Labels))
			for key, value := range claim.Labels {
				changed.Labels[key] = value
			}
			switch name {
			case "missing":
				delete(changed.Labels, boxDeletionLabel)
			case "corrupt":
				changed.Labels[boxDeletionLabel] = "unverified"
			case "legacy digest":
				legacy := changed
				legacy.Revision = ""
				legacy.LastUsedAt = ""
				delete(legacy.Labels, boxDeletionLabel)
				encoded, marshalErr := json.Marshal(legacy)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				changed.Labels[boxDeletionLabel] = fmt.Sprintf("%x", sha256.Sum256(encoded))
			case "owner":
				changed.RepoRoot = t.TempDir()
			case "resource":
				changed.CloudID = "bx_replacement"
				changed.Labels["box_id"] = changed.CloudID
			case "creation":
				changed.Labels[boxCreationLabel] = "2026-08-30T12:00:01Z"
			case "scope":
				t.Setenv("BOX_ORG", "other")
				changed.ProviderScope = (Provider{}).ClaimScope(testConfig())
				changed.Labels["ascii_box_scope"] = changed.ProviderScope
			}
			changed, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, changed)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil {
				t.Fatal("stale release snapshot accepted replacement claim")
			}
			if name != "revision" {
				if _, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true}); err == nil {
					t.Fatal("changed claim retained deletion authority")
				}
			}
			assertClaimRetained(t, changed)
			if len(f.deletedIDs) != 1 {
				t.Fatalf("replacement claim caused another deletion: %v", f.deletedIDs)
			}
		})
	}
}

func TestReleaseCompletedWitnessRetainsUncertainResource(t *testing.T) {
	for _, name := range []string{"lookup failure", "observed Box", "incomplete inventory", "matching inventory", "replacement inventory"} {
		t.Run(name, func(t *testing.T) {
			b, f, claim := completedDeletionFixture(t)
			lease, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "lookup failure":
				f.getHook = func(string) (boxData, error) { return boxData{}, errors.New("access denied") }
			case "observed Box":
				f.getHook = func(string) (boxData, error) { return f.box, nil }
			case "incomplete inventory":
				f.listHook = func() ([]boxData, error) { return nil, errors.New("incomplete inventory") }
			case "matching inventory":
				f.listHook = func() ([]boxData, error) { return []boxData{f.box}, nil }
			case "replacement inventory":
				replacement := f.box
				replacement.CreatedAt = "2026-08-30T12:00:01Z"
				f.listHook = func() ([]boxData, error) { return []boxData{replacement}, nil }
			}
			if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil {
				t.Fatal("uncertain resource finalized")
			}
			assertClaimRetained(t, claim)
			if len(f.deletedIDs) != 1 {
				t.Fatalf("uncertainty caused another deletion: %v", f.deletedIDs)
			}
		})
	}
}

func TestReleaseRetainsHiddenPendingDeletion(t *testing.T) {
	b, f, claim, lease := ownedFixture(t)
	f.releaseHook = func(string) error {
		f.deleted = true
		return errors.New("deletion operation is pending")
	}
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("pending native deletion succeeded")
	}
	assertClaimRetained(t, claim)
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("hidden pending deletion was mistaken for completed deletion")
	}
	assertClaimRetained(t, claim)
}

func TestBoxNotFoundRejectsIdentityErrors(t *testing.T) {
	for _, err := range []error{
		&boxIdentityError{id: "bx_404"},
		fmt.Errorf("lookup failed: %w", &boxIdentityError{id: "bx_404"}),
	} {
		if isNotFound(err) {
			t.Fatalf("identity mismatch classified as not-found: %v", err)
		}
	}
}

func TestResolveRequiresExactClaimAndRepositoryOwner(t *testing.T) {
	for _, name := range []string{"legacy", "foreign endpoint", "foreign organization", "other repository", "missing creation", "creation changed"} {
		t.Run(name, func(t *testing.T) {
			b, f, claim, _ := ownedFixture(t)
			req := ResolveRequest{ID: claim.CloudID, Repo: core.Repo{Root: claim.RepoRoot}}
			switch name {
			case "foreign endpoint":
				b.cfg.AsciiBox.BaseURL = "https://other.example"
			case "foreign organization":
				t.Setenv("BOX_ORG", "team-example")
			case "other repository":
				req.Repo.Root = t.TempDir()
			case "creation changed":
				f.box.CreatedAt = "2026-08-30T13:00:00Z"
			default:
				changed := claim
				changed.Labels = map[string]string{}
				for k, v := range claim.Labels {
					changed.Labels[k] = v
				}
				if name == "legacy" {
					changed.ProviderScope = "box:bx_1"
					changed.CloudID = ""
				} else {
					delete(changed.Labels, boxCreationLabel)
				}
				var err error
				claim, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, changed)
				if err != nil {
					t.Fatal(err)
				}
				req.ID = claim.LeaseID
			}
			if _, err := b.Resolve(context.Background(), req); err == nil {
				t.Fatal("unsafe reuse succeeded")
			}
			if len(f.prepareIDs) != 0 {
				t.Fatal("unowned SSH preparation")
			}
			assertClaimRetained(t, claim)
		})
	}
}

func TestLegacyStatusRemainsReadOnly(t *testing.T) {
	b, f, claim, _ := ownedFixture(t)
	changed := claim
	changed.ProviderScope = "box:bx_1"
	changed.CloudID = ""
	claim, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, changed)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: claim.LeaseID, StatusOnly: true, NoLocalStateMutations: true})
	if err != nil || lease.Server.CloudID != f.box.ID {
		t.Fatalf("status: err=%v", err)
	}
	if len(f.prepareIDs) != 0 {
		t.Fatal("status prepared SSH")
	}
	assertClaimRetained(t, claim)
	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err == nil {
		t.Fatal("status manufactured cleanup authority")
	}
}

func TestAcquirePublishesBeforeSSHAndRollsBackAfterCancellation(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprint(keep), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			f := &fakeAPI{box: testBox()}
			withFakeAPI(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var observed LeaseClaim
			f.prepareHook = func(string) error {
				claims, err := core.ListLeaseClaims()
				if err != nil || len(claims) != 1 {
					t.Fatalf("SSH before durable claim: count=%d err=%v", len(claims), err)
				}
				observed = claims[0]
				if _, err := boxClaimBinding(testConfig(), observed); err != nil {
					t.Fatal(err)
				}
				cancel()
				return errors.New("SSH setup failed")
			}
			b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
			_, err := b.Acquire(ctx, AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: keep})
			if err == nil {
				t.Fatal("setup succeeded")
			}
			if keep {
				assertClaimRetained(t, observed)
				if f.deleted {
					t.Fatal("Keep deleted resource")
				}
			} else {
				if !f.deleted {
					t.Fatal("cancelled acquire failed to clean original resource")
				}
				_, exists, err := core.ReadLeaseClaimWithPresence(observed.LeaseID)
				if err != nil || exists {
					t.Fatalf("cleanup claim exists=%t err=%v", exists, err)
				}
			}
		})
	}
}

func TestAcquireRollbackRetainsDeletionEvidence(t *testing.T) {
	for _, completion := range []string{"pending", "completed"} {
		t.Run(completion, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("BOX_ORG", "")
			f := &fakeAPI{box: testBox()}
			withFakeAPI(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var published LeaseClaim
			f.prepareHook = func(string) error {
				claims, err := core.ListLeaseClaims()
				if err != nil || len(claims) != 1 {
					t.Fatalf("SSH before durable claim: count=%d err=%v", len(claims), err)
				}
				published = claims[0]
				cancel()
				return errors.New("SSH setup failed")
			}
			deletions := 0
			f.releaseHook = func(id string) error {
				deletions++
				if completion == "pending" {
					f.deleted = true
					return &boxDeletionIncompleteError{operation: boxDeletionOperation{ID: testDeletionID, Kind: "box", TargetID: id, Status: "pending"}, err: context.DeadlineExceeded}
				}
				return nil
			}
			if completion == "completed" {
				f.listHook = func() ([]boxData, error) { return nil, errors.New("rollback inventory unavailable") }
			}
			b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
			if _, err := b.Acquire(ctx, AcquireRequest{Repo: core.Repo{Root: t.TempDir()}}); err == nil {
				t.Fatal("failed SSH setup succeeded")
			}
			var retained LeaseClaim
			if completion == "pending" {
				retained = assertPendingDeletionRetained(t, published, testDeletionID)
				f.deletionHook = func(targetID, operationID string) (boxDeletionOperation, error) {
					return boxDeletionOperation{ID: operationID, Kind: "box", TargetID: targetID, Status: "blocked"}, nil
				}
				if _, err := b.Resolve(context.Background(), ResolveRequest{ID: retained.LeaseID, ReleaseOnly: true}); err == nil {
					t.Fatal("blocked rollback operation authorized cleanup")
				}
				assertClaimRetained(t, retained)
				f.deletionHook = func(targetID, operationID string) (boxDeletionOperation, error) {
					return boxDeletionOperation{ID: operationID, Kind: "box", TargetID: targetID, Status: "completed", CompletedAt: "2026-09-02T09:00:00Z"}, nil
				}
			} else {
				retained = assertCompletedDeletionRetained(t, published)
			}
			f.listHook = nil
			lease, err := b.Resolve(context.Background(), ResolveRequest{ID: retained.LeaseID, ReleaseOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			teardown := false
			if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(context.Context, LeaseTarget) { teardown = true }}); err != nil {
				t.Fatal(err)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(retained.LeaseID); err != nil || exists {
				t.Fatalf("completed rollback claim remains: exists=%t err=%v", exists, err)
			}
			if deletions != 1 || len(f.prepareIDs) != 1 || teardown {
				t.Fatalf("retry repeated remote work: deletes=%d prepares=%v teardown=%t", deletions, f.prepareIDs, teardown)
			}
		})
	}
}

func TestRollbackRetainsChangedOrUnprovenAttempt(t *testing.T) {
	for _, name := range []string{"no creation event", "changed original ID", "missing timestamp", "changed generation", "wrong lease", "changed claim", "appeared claim"} {
		t.Run(name, func(t *testing.T) {
			b, f, claim, _ := ownedFixture(t)
			box := f.box
			leaseID := claim.LeaseID
			exists := true
			switch name {
			case "no creation event":
				box.createdID = ""
			case "changed original ID":
				box.createdID = "bx_other"
			case "missing timestamp":
				box.CreatedAt = nil
				f.box.CreatedAt = nil
			case "changed generation":
				box.CreatedAt = "2026-08-30T12:00:01Z"
				f.box.CreatedAt = box.CreatedAt
			case "wrong lease":
				leaseID = "cbx_abcdef123456"
			case "changed claim":
				changed := claim
				changed.RepoRoot = t.TempDir()
				if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, changed); err != nil {
					t.Fatal(err)
				}
			case "appeared claim":
				exists = false
			}
			err := b.rollbackBox(context.Background(), f, leaseID, box, claim, exists)
			if err == nil || f.deleted {
				t.Fatalf("rollback err=%v deleted=%t", err, f.deleted)
			}
		})
	}
}

func TestDecodeNewBoxPinsCreatedIdentity(t *testing.T) {
	for _, tail := range []string{
		`{"event":"ready","id":"bx_other","state":"ready","ip":"203.0.113.10"}`,
		`{"event":"error","id":"bx_other","error":"failed"}`,
		`{"event":"created","id":"bx_other"}`,
		`not json`,
		`{"event":"state","id":"bx_original","createdAt":"2026-08-30T13:00:00Z"}`,
	} {
		box, err := decodeNewBox("{\"event\":\"created\",\"id\":\"bx_original\",\"createdAt\":\"2026-08-30T12:00:00Z\"}\n" + tail)
		if err == nil || box.ID != "bx_original" || box.createdID != "bx_original" {
			t.Fatalf("retargeted creation: box=%+v err=%v", box, err)
		}
	}
	for _, out := range []string{`{"event":"error","id":"bx_external"}`, `{"event":"ready","id":"bx_external"}`, `{"event":"created","id":"current"}`} {
		box, err := decodeNewBox(out)
		if err == nil || box.createdID != "" {
			t.Fatalf("unproven creation: %+v err=%v", box, err)
		}
	}
}

func TestInventoryMustProveCompleteness(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"boxes":null}`, `{"boxes":[],"pageInfo":{"hasMore":true}}`, `{"boxes":[],"pageInfo":{"nextCursor":"next"}}`, `{"boxes":[],"pageInfo":{"hasMore":"false"}}`} {
		if _, err := decodeBoxes([]byte(raw), true); err == nil {
			t.Fatalf("accepted incomplete inventory %s", raw)
		}
	}
	for _, raw := range []string{`[]`, `{"boxes":[]}`, `{"boxes":[],"pageInfo":{"hasMore":false,"nextCursor":null}}`} {
		if boxes, err := decodeBoxes([]byte(raw), true); err != nil || len(boxes) != 0 {
			t.Fatalf("complete empty inventory: %s err=%v", raw, err)
		}
	}
}

func TestReleaseRevalidatesEveryMutation(t *testing.T) {
	for _, blocked := range []int{1, 2, 3, 4} {
		t.Run(fmt.Sprint(blocked), func(t *testing.T) {
			runner := &releaseCommandRunner{configPath: t.TempDir() + "/config.json", outcomes: map[string][]commandOutcome{
				"stop": {snapshotGuardOutcome()}, "delete": {snapshotGuardOutcome(), deletionOutcome(testDeletionID, "bx_guard", "box", "completed")}, "extend": {{result: LocalCommandResult{}}}, "info": {{result: LocalCommandResult{Stdout: `{"box":{"id":"bx_guard","state":"stopped"}}`}}},
			}}
			c := &client{apiKey: "box_test", apiURL: "https://ascii.dev", home: t.TempDir(), cliPath: "box", runner: runner, releasePollInterval: time.Nanosecond}
			calls := 0
			err := c.ReleaseBox(context.Background(), "bx_guard", func(context.Context) error {
				calls++
				if calls == blocked {
					return errors.New("ownership changed")
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "ownership changed") || calls != blocked {
				t.Fatalf("err=%v validations=%d", err, calls)
			}
			mutations := 0
			for _, command := range runner.commands {
				if strings.Contains(command, " stop ") || strings.Contains(command, " delete ") || strings.Contains(command, " extend ") {
					mutations++
				}
			}
			if mutations != blocked-1 {
				t.Fatalf("mutations=%d want %d", mutations, blocked-1)
			}
		})
	}
}

func TestCleanupTeardownUsesVerifiedTarget(t *testing.T) {
	b, f, claim, lease := ownedFixture(t)
	if b.ReleaseLeaseConnectionCleanupSafe() {
		t.Fatal("teardown must run inside ownership fence")
	}
	called := false
	err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease, GuardedRemoteCleanup: func(ctx context.Context, got LeaseTarget) {
		called = true
		if got.Server.CloudID != claim.CloudID || got.SSH.Host != boxHost(f.box) || ctx.Err() != nil {
			t.Fatal("wrong teardown target")
		}
	}})
	if err != nil || !called || !f.deleted {
		t.Fatalf("release err=%v teardown=%t deleted=%t", err, called, f.deleted)
	}
}

func TestEndpointRefreshCannotRetargetOwnership(t *testing.T) {
	_, f, claim, lease := ownedFixture(t)
	original := lease.Server
	for _, key := range []string{"provider", "box_id", "ascii_box_scope", boxCreationLabel} {
		server := original
		server.Labels = map[string]string{}
		for k, v := range original.Labels {
			server.Labels[k] = v
		}
		server.Labels[key] = "other"
		if _, err := (Provider{}).PrepareLeaseClaimEndpoint(claim, providerName, claim.Slug, server, false); err == nil {
			t.Fatalf("accepted changed %s", key)
		}
	}
	server := boxToServer(testConfig(), f.box, claim.LeaseID, claim.Slug, true)
	delete(server.Labels, boxCreationLabel)
	updated, err := (Provider{}).PrepareLeaseClaimEndpoint(claim, providerName, claim.Slug, server, false)
	if err != nil || updated.Labels[boxCreationLabel] != claim.Labels[boxCreationLabel] {
		t.Fatalf("refresh lost binding: %v", err)
	}
	server.Labels[boxDeletionLabel] = boxDeletionBinding(claim)
	server.Labels[boxDeletionOperationLabel] = testDeletionID
	server.Labels[boxDeletionOperationBindingLabel] = boxDeletionOperationBinding(claim, testDeletionID)
	updated, err = (Provider{}).PrepareLeaseClaimEndpoint(claim, providerName, claim.Slug, server, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{boxDeletionLabel, boxDeletionOperationLabel, boxDeletionOperationBindingLabel} {
		if _, exists := updated.Labels[key]; exists {
			t.Fatalf("endpoint refresh introduced unrecorded evidence: %s", key)
		}
	}

	t.Run("accepted deletion", func(t *testing.T) {
		_, f, pending := pendingDeletionFixture(t)
		server := boxToServer(testConfig(), f.box, pending.LeaseID, pending.Slug, true)
		updated, err := (Provider{}).PrepareLeaseClaimEndpoint(pending, providerName, pending.Slug, server, false)
		for _, key := range []string{boxDeletionOperationLabel, boxDeletionOperationBindingLabel} {
			if err != nil || updated.Labels[key] != pending.Labels[key] {
				t.Fatalf("refresh lost accepted reference %s: %v", key, err)
			}
			server.Labels[key] = "replacement"
			if _, err := (Provider{}).PrepareLeaseClaimEndpoint(pending, providerName, pending.Slug, server, false); err == nil {
				t.Fatalf("endpoint refresh replaced %s", key)
			}
			delete(server.Labels, key)
		}
	})

	t.Run("completed deletion", func(t *testing.T) {
		_, f, completed := completedDeletionFixture(t)
		server := boxToServer(testConfig(), f.box, completed.LeaseID, completed.Slug, true)
		updated, err := (Provider{}).PrepareLeaseClaimEndpoint(completed, providerName, completed.Slug, server, false)
		if err != nil || updated.Labels[boxDeletionLabel] != completed.Labels[boxDeletionLabel] {
			t.Fatalf("refresh lost completed deletion witness: %v", err)
		}
		server.Labels[boxDeletionLabel] = "replacement"
		if _, err := (Provider{}).PrepareLeaseClaimEndpoint(completed, providerName, completed.Slug, server, false); err == nil {
			t.Fatal("endpoint refresh replaced the deletion witness")
		}
	})
}

// Keep this compile-time check near cleanup tests: the provider owns teardown.
var _ interface{ ReleaseLeaseConnectionCleanupSafe() bool } = (*backend)(nil)

func TestAcquirePreservesUnconfirmedCreateFailureDiagnostic(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeAPI{createErr: errors.New("native quota exhausted (429)")}
	withFakeAPI(t, f)
	b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	_, err := b.Acquire(context.Background(), AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err == nil {
		t.Fatal("failed creation succeeded")
	}
	diagnostic := err.Error()
	var exitErr core.ExitError
	if core.AsExitError(err, &exitErr) {
		diagnostic = exitErr.Message
	}
	if !strings.Contains(diagnostic, "native quota exhausted (429)") {
		t.Fatalf("CLI diagnostic hid the provider error: %s", diagnostic)
	}
	if len(f.deletedIDs) != 0 || len(f.prepareIDs) != 0 {
		t.Fatal("unconfirmed creation authorized mutation")
	}
}

func TestReadOnlyInventoryAcceptsPaginationWithoutProvingAbsence(t *testing.T) {
	raw := []byte(`{"boxes":[],"pageInfo":{"hasMore":true,"nextCursor":"next"}}`)
	if _, err := decodeBoxes(raw, false); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeBoxes(raw, true); err == nil {
		t.Fatal("paginated read proved absence")
	}
	for _, raw := range []string{`{"boxes":[{}]}`, `{"boxes":[{"id":"current"}]}`, `{"boxes":[{"id":"bx_1"},{"id":"bx_1"}]}`} {
		if _, err := decodeBoxes([]byte(raw), true); err == nil {
			t.Fatalf("malformed inventory proved absence: %s", raw)
		}
	}
}

func TestAcquireCallbackFailureUsesOriginalClaim(t *testing.T) {
	for _, mode := range []string{"disposable", "keep", "changed claim"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			f := &fakeAPI{box: testBox()}
			withFakeAPI(t, f)
			stubSSHWait(t)
			b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
			var expected LeaseClaim
			_, err := b.Acquire(context.Background(), AcquireRequest{Keep: mode == "keep", Repo: core.Repo{Root: t.TempDir()}, OnAcquired: func(lease LeaseTarget) error {
				claim, exists, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
				if err != nil || !exists {
					t.Fatal("callback before publication")
				}
				expected = claim
				if mode == "changed claim" {
					changed := claim
					changed.RepoRoot = t.TempDir()
					expected, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, changed)
					if err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("callback failed")
			}})
			if err == nil || !strings.Contains(err.Error(), "callback failed") {
				t.Fatalf("err=%v", err)
			}
			if mode == "disposable" {
				if !f.deleted {
					t.Fatal("callback leaked disposable Box")
				}
			} else {
				if f.deleted {
					t.Fatal("deleted retained/successor Box")
				}
				assertClaimRetained(t, expected)
			}
		})
	}
}

func TestClientInfoAllowsReadOnlyAliasesButPinsConcreteRequests(t *testing.T) {
	for _, id := range []string{"current", "self", "friendly-name", "bx_original"} {
		t.Run(id, func(t *testing.T) {
			runner := &releaseCommandRunner{configPath: t.TempDir() + "/config.json", outcomes: map[string][]commandOutcome{"info": {{result: LocalCommandResult{Stdout: `{"box":{"id":"bx_resolved"}}`}}}}}
			c := &client{apiKey: "box_test", apiURL: "https://ascii.dev", home: t.TempDir(), cliPath: "box", runner: runner}
			box, err := c.GetBox(context.Background(), id)
			if concreteBoxID(id) {
				if err == nil {
					t.Fatal("concrete request was retargeted")
				}
			} else if err != nil || box.ID != "bx_resolved" {
				t.Fatalf("read-only alias failed: id=%s err=%v", id, err)
			}
		})
	}
}
