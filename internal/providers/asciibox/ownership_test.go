package asciibox

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
	for _, name := range []string{"missing", "corrupt", "owner", "resource", "creation", "scope", "revision"} {
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

func TestRollbackRetainsChangedOrUnprovenAttempt(t *testing.T) {
	for _, name := range []string{"no creation event", "changed original ID", "missing timestamp", "changed claim", "appeared claim"} {
		t.Run(name, func(t *testing.T) {
			b, f, claim, _ := ownedFixture(t)
			box := f.box
			exists := true
			switch name {
			case "no creation event":
				box.createdID = ""
			case "changed original ID":
				box.createdID = "bx_other"
			case "missing timestamp":
				box.CreatedAt = nil
				f.box.CreatedAt = nil
			case "changed claim":
				changed := claim
				changed.RepoRoot = t.TempDir()
				if err := core.ReplaceLeaseClaimIfUnchanged(claim.LeaseID, claim, changed); err != nil {
					t.Fatal(err)
				}
			case "appeared claim":
				exists = false
			}
			err := b.rollbackBox(context.Background(), f, claim.LeaseID, box, claim, exists)
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
				"stop": {snapshotGuardOutcome()}, "delete": {snapshotGuardOutcome(), {result: LocalCommandResult{}}}, "extend": {{result: LocalCommandResult{}}}, "info": {{result: LocalCommandResult{Stdout: `{"box":{"id":"bx_guard","state":"stopped"}}`}}},
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
	updated, err = (Provider{}).PrepareLeaseClaimEndpoint(claim, providerName, claim.Slug, server, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := updated.Labels[boxDeletionLabel]; exists {
		t.Fatal("endpoint refresh introduced an unrecorded deletion witness")
	}

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
