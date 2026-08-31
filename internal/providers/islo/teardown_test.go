package islo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
)

const isloTeardownLeaseID = "isb_crabbox-repo-abcdef"
const isloTeardownName = "crabbox-repo-abcdef"

func newIsloTeardownBackend(t *testing.T, client *fakeIsloSyncClient, stderr io.Writer) *isloBackend {
	t.Helper()
	restore := swapNewIsloClient(client)
	t.Cleanup(restore)
	return &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test"}},
		rt:  Runtime{Stdout: io.Discard, Stderr: stderr},
	}
}

func requireIsloClaimRetained(t *testing.T, leaseID string) {
	t.Helper()
	if _, ok, err := resolveExactIsloLeaseClaim(leaseID); err != nil || !ok {
		t.Fatalf("recovery claim ok=%t err=%v, want the claim retained for a retry", ok, err)
	}
}

func requireIsloClaimDropped(t *testing.T, leaseID string) {
	t.Helper()
	if _, ok, err := resolveExactIsloLeaseClaim(leaseID); err != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want the claim dropped after a proven delete", ok, err)
	}
}

// claimIsloLegacyLease writes a claim in the shape used before the identity
// binding existed: no resource id, no scope, no creator labels.
func claimIsloLegacyLease(t *testing.T, leaseID string) {
	t.Helper()
	if err := claimLeaseForRepoProvider(leaseID, "web", isloProvider, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
}

// TestIsloStopConfirmsTeardownWithTombstone pins the strong path: the exact
// resource id from the claim is deleted and the by-id tombstone proves it.
func TestIsloStopConfirmsTeardownWithTombstone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
	client := &fakeIsloSyncClient{createdBy: isloTestKeyName}
	client.registerSandbox(isloTeardownName, isloTestResourceID)
	var stderr bytes.Buffer
	backend := newIsloTeardownBackend(t, client, &stderr)

	if err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID}); err != nil {
		t.Fatal(err)
	}
	if client.deleteCalls != 1 {
		t.Fatalf("delete calls=%d, want 1", client.deleteCalls)
	}
	if len(client.byIDCalls) == 0 || client.byIDCalls[len(client.byIDCalls)-1] != isloTestResourceID {
		t.Fatalf("by-id calls=%#v, want the tombstone of the claimed resource", client.byIDCalls)
	}
	if !strings.Contains(stderr.String(), "proof="+string(isloProofTombstone)) {
		t.Fatalf("stderr=%q, want the tombstone proof reported", stderr.String())
	}
	requireIsloClaimDropped(t, isloTeardownLeaseID)
}

// TestIsloStopIsIdempotentOnAnAlreadyDeletedSandbox covers the most likely
// real-world retry: the sandbox is already gone, the claim records its id, and
// the tombstone releases the lease without issuing a second delete.
func TestIsloStopIsIdempotentOnAnAlreadyDeletedSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
	client := &fakeIsloSyncClient{createdBy: isloTestKeyName}
	client.registerSandbox(isloTeardownName, isloTestResourceID)
	client.markDeleted(isloTeardownName)
	var stderr bytes.Buffer
	backend := newIsloTeardownBackend(t, client, &stderr)

	if err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID}); err != nil {
		t.Fatal(err)
	}
	if client.deleteCalls != 0 {
		t.Fatalf("delete calls=%d, want 0: the resource was already tombstoned", client.deleteCalls)
	}
	if !strings.Contains(stderr.String(), "proof="+string(isloProofTombstone)) {
		t.Fatalf("stderr=%q, want the tombstone proof reported", stderr.String())
	}
	requireIsloClaimDropped(t, isloTeardownLeaseID)
}

// TestIsloStopConfirmsTeardownWithExactNameNotFound covers the fallback proof:
// no tombstone is available, but the resource was observed under these
// credentials and its exact name answers 404 right after the delete.
func TestIsloStopConfirmsTeardownWithExactNameNotFound(t *testing.T) {
	for name, tc := range map[string]struct {
		resourceID string
		byID       map[string]*gosdk.SandboxResponse
	}{
		"claim predates the identity binding": {},
		"tombstone not visible to us": {
			resourceID: isloTestResourceID,
			byID:       map[string]*gosdk.SandboxResponse{isloTestResourceID: nil},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			if tc.resourceID == "" {
				claimIsloLegacyLease(t, isloTeardownLeaseID)
			} else {
				claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, tc.resourceID, isloTestClaimScope)
			}
			client := &fakeIsloSyncClient{byID: tc.byID}
			if tc.resourceID != "" {
				client.registerSandbox(isloTeardownName, tc.resourceID)
			}
			var stderr bytes.Buffer
			backend := newIsloTeardownBackend(t, client, &stderr)

			if err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID}); err != nil {
				t.Fatal(err)
			}
			if client.deleteCalls != 1 {
				t.Fatalf("delete calls=%d, want 1", client.deleteCalls)
			}
			if !strings.Contains(stderr.String(), "proof="+string(isloProofNameAbsent)+"\n") {
				t.Fatalf("stderr=%q, want the exact-name proof reported", stderr.String())
			}
			requireIsloClaimDropped(t, isloTeardownLeaseID)
		})
	}
}

// TestIsloTeardownDeletesTheNameTheClaimedIDResolvesTo pins that DELETE, which
// is name-only, is aimed at the name the authoritative by-id response reports
// rather than the name the caller happened to derive locally.
func TestIsloTeardownDeletesTheNameTheClaimedIDResolvesTo(t *testing.T) {
	const liveName = "crabbox-repo-fedcba"
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
	client := &fakeIsloSyncClient{createdBy: isloTestKeyName}
	// The locally recorded name no longer answers, but the claimed resource is
	// alive and reports the name it actually has.
	client.registerSandbox(liveName, isloTestResourceID)
	client.markDeleted(isloTeardownName)
	var stderr bytes.Buffer
	backend := newIsloTeardownBackend(t, client, &stderr)

	if err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID}); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedNames) != 1 || client.deletedNames[0] != liveName {
		t.Fatalf("deleted names=%#v, want exactly the name the claimed id resolves to (%q)", client.deletedNames, liveName)
	}
	// The success line must name what was deleted, not what the caller asked
	// about, or it reports a different sandbox than the one that is gone.
	if !strings.Contains(stderr.String(), "sandbox="+liveName+" proof="+string(isloProofTombstone)) {
		t.Fatalf("stderr=%q, want the deleted name and the tombstone proof reported", stderr.String())
	}
	requireIsloClaimDropped(t, isloTeardownLeaseID)
}

// TestIsloTeardownDeletesWhenTheIdentityReadsFail is the resource-safety
// property: a sandbox that exists is billed for, so a pre-flight read that
// merely fails must never suppress the delete. Only a positive identity
// mismatch may do that.
func TestIsloTeardownDeletesWhenTheIdentityReadsFail(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
	unavailable := errors.New("503 service unavailable")
	client := &fakeIsloSyncClient{byIDErr: unavailable, getSandboxErr: unavailable}
	var stderr bytes.Buffer
	backend := newIsloTeardownBackend(t, client, &stderr)

	err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID})
	if err == nil || !strings.Contains(err.Error(), "confirm sandbox deletion by id") {
		t.Fatalf("err=%v, want an unproven teardown after the confirmation read failed", err)
	}
	if client.deleteCalls != 1 || client.deletedNames[0] != isloTeardownName {
		t.Fatalf("delete calls=%d names=%#v, want the delete issued despite the failed reads", client.deleteCalls, client.deletedNames)
	}
	if !strings.Contains(stderr.String(), "could not read sandbox") {
		t.Fatalf("stderr=%q, want the failed identity reads reported", stderr.String())
	}
	requireIsloClaimRetained(t, isloTeardownLeaseID)
}

// TestIsloTeardownToleratesACreatorAttributionDifference pins that attribution
// never strands a lease. created_by is the API key's name, which only
// corroborates ownership, so a difference is advisory: the teardown proceeds
// and says so.
func TestIsloTeardownToleratesACreatorAttributionDifference(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
	client := &fakeIsloSyncClient{createdBy: "other-key"}
	client.registerSandbox(isloTeardownName, isloTestResourceID)
	var stderr bytes.Buffer
	backend := newIsloTeardownBackend(t, client, &stderr)

	if err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID}); err != nil {
		t.Fatalf("err=%v, want the teardown to proceed despite the attribution difference", err)
	}
	if client.deleteCalls != 1 {
		t.Fatalf("delete calls=%d, want 1", client.deleteCalls)
	}
	if !strings.Contains(stderr.String(), "advisory only") || !strings.Contains(stderr.String(), "other-key") {
		t.Fatalf("stderr=%q, want the creator difference reported as advisory", stderr.String())
	}
	requireIsloClaimDropped(t, isloTeardownLeaseID)
}

// TestIsloStopReleasesAClaimWithNoRecordedResourceID keeps a claim written
// before the identity binding from becoming permanently unreleasable. No
// stronger evidence than the name-404 can exist for such a claim, so it is
// accepted and reported under its own weaker proof.
func TestIsloStopReleasesAClaimWithNoRecordedResourceID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	claimIsloLegacyLease(t, isloTeardownLeaseID)
	client := &fakeIsloSyncClient{getSandboxGone: true}
	var stderr bytes.Buffer
	backend := newIsloTeardownBackend(t, client, &stderr)

	if err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID}); err != nil {
		t.Fatalf("err=%v, want a legacy claim with an already-absent name to be releasable", err)
	}
	if client.deleteCalls != 0 {
		t.Fatalf("delete calls=%d, want 0: the name was already absent", client.deleteCalls)
	}
	if !strings.Contains(stderr.String(), "proof="+string(isloProofNameAbsentUnbound)) {
		t.Fatalf("stderr=%q, want the weaker unbound proof reported", stderr.String())
	}
	if !strings.Contains(stderr.String(), "records no resource id") {
		t.Fatalf("stderr=%q, want the weaker evidence warned about", stderr.String())
	}
	requireIsloClaimDropped(t, isloTeardownLeaseID)
}

// TestIsloStopRetainsClaimWhenTeardownIsUncertain is the core safety property:
// anything short of proof leaves the recovery claim in place so a later
// `crabbox stop` can finish the job.
func TestIsloStopRetainsClaimWhenTeardownIsUncertain(t *testing.T) {
	for name, tc := range map[string]struct {
		resourceID  string
		client      func() *fakeIsloSyncClient
		wantErr     string
		wantMessage string
		wantDeletes int
	}{
		"delete call fails": {
			resourceID: isloTestResourceID,
			client: func() *fakeIsloSyncClient {
				return &fakeIsloSyncClient{deleteErr: errors.New("connection reset")}
			},
			wantErr:     "islo delete sandbox",
			wantDeletes: 1,
		},
		"resource still running after delete": {
			resourceID: isloTestResourceID,
			client: func() *fakeIsloSyncClient {
				return &fakeIsloSyncClient{byID: map[string]*gosdk.SandboxResponse{
					isloTestResourceID: {ID: isloTestResourceID, Name: isloTeardownName, Status: "running"},
				}}
			},
			wantErr:     "still reports status",
			wantMessage: "a delete was issued",
			wantDeletes: 1,
		},
		"tombstone lookup fails": {
			resourceID: isloTestResourceID,
			client: func() *fakeIsloSyncClient {
				return &fakeIsloSyncClient{byIDErr: errors.New("i/o timeout")}
			},
			wantErr:     "confirm sandbox deletion by id",
			wantDeletes: 1,
		},
		"name still resolves after delete": {
			client: func() *fakeIsloSyncClient {
				return &fakeIsloSyncClient{getSandbox: &gosdk.SandboxResponse{Name: isloTeardownName, Status: "running"}}
			},
			wantErr:     "still resolves by name",
			wantMessage: "a delete was issued",
			wantDeletes: 1,
		},
		// The claim names a resource generation, but nothing under these
		// credentials could see it, so a 404 proves nothing. The message must
		// not claim a delete that never went out.
		"claimed resource is unreachable and its name is absent": {
			resourceID: isloTestResourceID,
			client: func() *fakeIsloSyncClient {
				return &fakeIsloSyncClient{
					byID:           map[string]*gosdk.SandboxResponse{isloTestResourceID: nil},
					getSandboxGone: true,
				}
			},
			wantErr:     "cannot prove",
			wantMessage: "no delete was issued",
			wantDeletes: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			if tc.resourceID == "" {
				claimIsloLegacyLease(t, isloTeardownLeaseID)
			} else {
				claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, tc.resourceID, isloTestClaimScope)
			}
			client := tc.client()
			if tc.resourceID != "" {
				client.registerSandbox(isloTeardownName, tc.resourceID)
			}
			backend := newIsloTeardownBackend(t, client, io.Discard)

			err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
			if tc.wantMessage != "" && !strings.Contains(err.Error(), tc.wantMessage) {
				t.Fatalf("err=%v, want it to state %q", err, tc.wantMessage)
			}
			if client.deleteCalls != tc.wantDeletes {
				t.Fatalf("delete calls=%d, want %d", client.deleteCalls, tc.wantDeletes)
			}
			requireIsloClaimRetained(t, isloTeardownLeaseID)
		})
	}
}

// TestIsloRunCleanupReleaseKeepsAnAdoptableClaim covers the run-cleanup defer
// directly: it proves the delete before dropping the claim, and it keeps the
// claim when it cannot.
func TestIsloRunCleanupReleaseKeepsAnAdoptableClaim(t *testing.T) {
	for name, tc := range map[string]struct {
		client      func() *fakeIsloSyncClient
		wantErr     string
		wantDeletes int
		wantClaim   bool
	}{
		"proven delete drops the claim": {
			client: func() *fakeIsloSyncClient {
				client := &fakeIsloSyncClient{createdBy: isloTestKeyName}
				client.registerSandbox(isloTeardownName, isloTestResourceID)
				return client
			},
			wantDeletes: 1,
		},
		"unproven delete keeps the claim": {
			client: func() *fakeIsloSyncClient {
				client := &fakeIsloSyncClient{byIDErr: errors.New("i/o timeout")}
				client.registerSandbox(isloTeardownName, isloTestResourceID)
				return client
			},
			wantErr:     "confirm sandbox deletion by id",
			wantDeletes: 1,
			wantClaim:   true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
			client := tc.client()
			backend := newIsloTeardownBackend(t, client, io.Discard)

			err := backend.releaseIsloLease(client, isloTeardownLeaseID, isloTeardownName)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err=%v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
			if client.deleteCalls != tc.wantDeletes {
				t.Fatalf("delete calls=%d, want %d", client.deleteCalls, tc.wantDeletes)
			}
			if tc.wantClaim {
				requireIsloClaimRetained(t, isloTeardownLeaseID)
				return
			}
			requireIsloClaimDropped(t, isloTeardownLeaseID)
		})
	}
}

// TestIsloRunCleanupDeletesWhenTheClaimIsGone keeps a lost claim from turning
// into a leaked, billed sandbox. There is no identity left to fence on, so the
// defer falls back to the unconditional name delete it always performed.
func TestIsloRunCleanupDeletesWhenTheClaimIsGone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	client := &fakeIsloSyncClient{}
	var stderr bytes.Buffer
	backend := newIsloTeardownBackend(t, client, &stderr)

	if err := backend.releaseIsloLease(client, isloTeardownLeaseID, isloTeardownName); err != nil {
		t.Fatal(err)
	}
	if client.deleteCalls != 1 || client.deletedNames[0] != isloTeardownName {
		t.Fatalf("delete calls=%d names=%#v, want the sandbox deleted rather than leaked", client.deleteCalls, client.deletedNames)
	}
	if !strings.Contains(stderr.String(), "has no exact local claim") {
		t.Fatalf("stderr=%q, want the missing claim reported", stderr.String())
	}
}

// TestIsloRunCleanupBoundsTheWholeTeardown pins the latency contract of the run
// cleanup defer: the user waits on it, so a control plane that answers nothing
// costs them one cleanup budget in total, not one per API call.
func TestIsloRunCleanupBoundsTheWholeTeardown(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	const budget = 100 * time.Millisecond
	withIsloCleanupTimeout(t, budget)
	claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
	client := &fakeIsloSyncClient{blockReads: true, blockDelete: true}
	client.registerSandbox(isloTeardownName, isloTestResourceID)
	backend := newIsloTeardownBackend(t, client, io.Discard)

	// Four calls starve here: the two pre-flight identity reads, the DELETE, and
	// the tombstone confirmation. A per-call budget would cost ~4x, and a call
	// left unbounded would never return at all, so the wait is what is asserted.
	done := make(chan error, 1)
	go func() { done <- backend.releaseIsloLease(client, isloTeardownLeaseID, isloTeardownName) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(2 * budget):
		t.Fatalf("cleanup did not return within %s, want the whole teardown bounded by one %s budget", 2*budget, budget)
	}
	if err == nil {
		t.Fatal("err=nil, want an unproven teardown when nothing answers")
	}
	// The delete still has to be attempted: an undeleted sandbox keeps billing.
	if client.deleteCalls != 1 {
		t.Fatalf("delete calls=%d, want the delete attempted within the reserved slice of the budget", client.deleteCalls)
	}
	// Counting the call is not enough. Without the reserved slice the pre-flight
	// reads consume the whole budget and the DELETE is dispatched on an already
	// cancelled context, which a real transport refuses to send - the sandbox
	// then keeps billing even though deleteCalls says the delete was attempted.
	if len(client.deleteCtxErrs) != 1 {
		t.Fatalf("delete context observations=%d, want exactly one", len(client.deleteCtxErrs))
	}
	if client.deleteCtxErrs[0] != nil {
		t.Fatalf("delete dispatched on an expired context (%v), want the reserved slice of the budget to keep it live", client.deleteCtxErrs[0])
	}
	requireIsloClaimRetained(t, isloTeardownLeaseID)
}

// TestIsloTeardownIgnoresEventuallyConsistentList pins that absence is never
// argued from `GET /sandboxes`. The live listing kept returning deleted
// sandboxes for seconds after the by-name lookup already answered 404, so a
// teardown that consulted it would either hang or, worse, conclude the opposite
// of the truth.
func TestIsloTeardownIgnoresEventuallyConsistentList(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
	client := &fakeIsloSyncClient{
		// The stale listing entry a real tenant sees for a few seconds after
		// the delete has already been confirmed authoritatively.
		listResponse: []*gosdk.SandboxResponse{{ID: isloTestResourceID, Name: isloTeardownName, Status: "running"}},
	}
	client.registerSandbox(isloTeardownName, isloTestResourceID)
	backend := newIsloTeardownBackend(t, client, io.Discard)

	if err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID}); err != nil {
		t.Fatal(err)
	}
	if client.listCalls != 0 {
		t.Fatalf("list calls=%d during teardown, want 0: a list cannot prove absence", client.listCalls)
	}
	requireIsloClaimDropped(t, isloTeardownLeaseID)
	// The listing still reports the deleted sandbox. That lag is exactly why the
	// teardown proof above may not be derived from it.
	servers, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("list=%#v, want the stale entry the live API keeps returning", servers)
	}
}

func TestIsloStopRefusesTeardownOutsideTheClaimedScope(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, "endpoint:https://other.example")
	client := &fakeIsloSyncClient{}
	client.registerSandbox(isloTeardownName, isloTestResourceID)
	backend := newIsloTeardownBackend(t, client, io.Discard)

	err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID})
	if err == nil || !strings.Contains(err.Error(), "refusing to act") {
		t.Fatalf("err=%v, want a foreign-scope refusal", err)
	}
	if client.deleteCalls != 0 {
		t.Fatalf("delete calls=%d, want 0", client.deleteCalls)
	}
	requireIsloClaimRetained(t, isloTeardownLeaseID)
}

// TestIsloStopRefusesAForeignResourceUnderTheClaimedName is the one case where
// a failed identity check does fence the delete: the name resolves to a
// different resource id than the lease owns, which positively proves the target
// is not ours. Whether the live API ever hands a released name to a new sandbox
// is UNCONFIRMED; if it never does, this guard simply never fires.
func TestIsloStopRefusesAForeignResourceUnderTheClaimedName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
	client := &fakeIsloSyncClient{}
	client.registerSandbox(isloTeardownName, "0195f3d2-5c1a-7c39-9c1e-000000000000")
	backend := newIsloTeardownBackend(t, client, io.Discard)

	err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID})
	if err == nil || !strings.Contains(err.Error(), "this lease does not own") {
		t.Fatalf("err=%v, want a foreign-resource refusal", err)
	}
	if client.deleteCalls != 0 {
		t.Fatalf("delete calls=%d, want 0", client.deleteCalls)
	}
	requireIsloClaimRetained(t, isloTeardownLeaseID)
}
