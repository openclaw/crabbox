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
	return newIsloForgetMissingTeardownBackend(t, client, stderr, false)
}

// newIsloForgetMissingTeardownBackend builds the same backend with the
// operator opt-out set either way, so every test states which side of the
// fail-closed gate it is pinning.
func newIsloForgetMissingTeardownBackend(t *testing.T, client *fakeIsloSyncClient, stderr io.Writer, forgetMissing bool) *isloBackend {
	t.Helper()
	restore := swapNewIsloClient(client)
	t.Cleanup(restore)
	return &isloBackend{
		cfg: Config{Islo: IsloConfig{APIKey: "test", ForgetMissing: forgetMissing}},
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

// TestIsloTeardownRefusesABlindNameDeleteForAnIDBoundClaim is the resource
// boundary this identity binding exists to hold. DELETE is name-only and an
// Islo sandbox name is reusable once its sandbox is deleted, so when both
// identity reads fail there is nothing showing that the recorded name is still
// the resource the lease owns - deleting it could destroy a different tenant
// sandbox that has since taken the name. The teardown must fail closed and keep
// the claim instead.
func TestIsloTeardownRefusesABlindNameDeleteForAnIDBoundClaim(t *testing.T) {
	for name, tc := range map[string]struct {
		client func() *fakeIsloSyncClient
	}{
		// The control-plane outage the fail-closed path exists for: neither
		// lookup answers, so nothing identifies the name.
		"both identity reads fail": {
			client: func() *fakeIsloSyncClient {
				unavailable := errors.New("503 service unavailable")
				return &fakeIsloSyncClient{byIDErr: unavailable, getSandboxErr: unavailable}
			},
		},
		// The claimed resource is invisible to these credentials and the name
		// read fails, so again nothing ties the name to the claimed id.
		"claimed id is not visible and the name read fails": {
			client: func() *fakeIsloSyncClient {
				return &fakeIsloSyncClient{
					byID:          map[string]*gosdk.SandboxResponse{isloTestResourceID: nil},
					getSandboxErr: errors.New("i/o timeout"),
				}
			},
		},
		// The name answers, but with no resource id, so it shows only that
		// something holds the name - not that it is still our sandbox.
		"the name answers without a resource id": {
			client: func() *fakeIsloSyncClient {
				return &fakeIsloSyncClient{
					byID:       map[string]*gosdk.SandboxResponse{isloTestResourceID: nil},
					getSandbox: &gosdk.SandboxResponse{Name: isloTeardownName, Status: "running"},
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
			client := tc.client()
			var stderr bytes.Buffer
			backend := newIsloTeardownBackend(t, client, &stderr)

			err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID})
			if err == nil || !strings.Contains(err.Error(), "refused to delete sandbox") {
				t.Fatalf("err=%v, want the delete refused because the target could not be identified", err)
			}
			// The refusal has to be actionable: an undeleted sandbox keeps
			// billing, so the operator must be told the sandbox may still be
			// running and exactly which command finishes the job.
			for _, want := range []string{
				isloTeardownName,
				isloTestResourceID,
				"may still be running and billable",
				isloCleanupCommand(isloTeardownLeaseID),
				// A claim whose sandbox can never be read again would
				// otherwise be unstickable, so the refusal an operator
				// actually sees has to name the way out.
				"--islo-forget-missing",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err=%v, want it to state %q", err, want)
				}
			}
			if client.deleteCalls != 0 {
				t.Fatalf("delete calls=%d names=%#v, want no delete: the recorded name was never shown to be our resource", client.deleteCalls, client.deletedNames)
			}
			requireIsloClaimRetained(t, isloTeardownLeaseID)
		})
	}
}

// TestIsloForgetMissingDropsAnUnverifiableClaimWithoutDeleting covers the
// operator escape hatch from the fail-closed gate. The gate is correct but it
// can leave a claim that no retry will ever satisfy - a sandbox whose id these
// credentials can never read again - and with no way to drop it the lease is
// stuck forever. islo forget-missing drops the claim on explicit request and
// still issues NO delete, so the resource boundary the gate protects is never
// crossed: the trade is an untracked, possibly billing sandbox, and it has to be
// stated on stderr rather than happening quietly.
func TestIsloForgetMissingDropsAnUnverifiableClaimWithoutDeleting(t *testing.T) {
	for name, tc := range map[string]struct {
		client func() *fakeIsloSyncClient
	}{
		"both identity reads fail": {
			client: func() *fakeIsloSyncClient {
				unavailable := errors.New("503 service unavailable")
				return &fakeIsloSyncClient{byIDErr: unavailable, getSandboxErr: unavailable}
			},
		},
		"claimed id is not visible and the name read fails": {
			client: func() *fakeIsloSyncClient {
				return &fakeIsloSyncClient{
					byID:          map[string]*gosdk.SandboxResponse{isloTestResourceID: nil},
					getSandboxErr: errors.New("i/o timeout"),
				}
			},
		},
		"the name answers without a resource id": {
			client: func() *fakeIsloSyncClient {
				return &fakeIsloSyncClient{
					byID:       map[string]*gosdk.SandboxResponse{isloTestResourceID: nil},
					getSandbox: &gosdk.SandboxResponse{Name: isloTeardownName, Status: "running"},
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
			client := tc.client()
			var stderr bytes.Buffer
			backend := newIsloForgetMissingTeardownBackend(t, client, &stderr, true)

			if err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID}); err != nil {
				t.Fatalf("err=%v, want the claim dropped on explicit operator request", err)
			}
			// The opt-out only forgets. Deleting a name that was never
			// identified is the exact thing the gate exists to prevent, and
			// acknowledging a stuck claim must not buy that back.
			if client.deleteCalls != 0 {
				t.Fatalf("delete calls=%d names=%#v, want 0: forget-missing drops the claim, it does not authorize a blind delete", client.deleteCalls, client.deletedNames)
			}
			requireIsloClaimDropped(t, isloTeardownLeaseID)
			// Dropping the claim discards the only handle on a resource that
			// may still be charging the tenant, so the action has to be
			// auditable from the terminal it ran in.
			for _, want := range []string{
				"WITHOUT deleting sandbox",
				isloTeardownName,
				isloTestResourceID,
				"may still exist and may still be billing",
				"no longer track it",
				"proof=" + string(isloProofUnverifiedForgotten),
			} {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr=%q, want it to state %q", stderr.String(), want)
				}
			}
		})
	}
}

// TestIsloForgetMissingDoesNotReachTheRunCleanupDefer bounds the opt-out to the
// teardown the operator is actually standing in front of. The run cleanup defer
// knows its sandbox exists - this run created it moments ago - so retaining an
// adoptable recovery claim always beats forgetting one, and an environment-wide
// acknowledgement must not silently turn every failed cleanup into a leaked,
// untracked sandbox.
func TestIsloForgetMissingDoesNotReachTheRunCleanupDefer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
	unavailable := errors.New("503 service unavailable")
	client := &fakeIsloSyncClient{byIDErr: unavailable, getSandboxErr: unavailable}
	var stderr bytes.Buffer
	backend := newIsloForgetMissingTeardownBackend(t, client, &stderr, true)

	err := backend.releaseIsloLease(client, isloTeardownLeaseID, isloTeardownName)
	if err == nil || !strings.Contains(err.Error(), "refused to delete sandbox") {
		t.Fatalf("err=%v, want run cleanup to stay fail-closed with forget-missing set", err)
	}
	if client.deleteCalls != 0 {
		t.Fatalf("delete calls=%d names=%#v, want 0", client.deleteCalls, client.deletedNames)
	}
	if strings.Contains(stderr.String(), string(isloProofUnverifiedForgotten)) {
		t.Fatalf("stderr=%q, want no unverified-forget release from the run cleanup defer", stderr.String())
	}
	requireIsloClaimRetained(t, isloTeardownLeaseID)
}

// TestIsloForgetMissingLeavesEveryOtherTeardownPathAlone pins the blast radius
// of the opt-out: it may only convert the one unverifiable-identity refusal into
// a claim drop. A proven teardown, the pre-binding name-only delete, the
// positive identity mismatch, and an unproven confirmation must all behave
// exactly as they do without it - otherwise the flag quietly becomes "release
// this lease no matter what the API said".
func TestIsloForgetMissingLeavesEveryOtherTeardownPathAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		legacyClaim bool
		client      func() *fakeIsloSyncClient
		wantErr     string
		wantDeletes int
		wantProof   isloTeardownProof
		wantClaim   bool
	}{
		"a verified sandbox is still deleted and proven by tombstone": {
			client: func() *fakeIsloSyncClient {
				client := &fakeIsloSyncClient{createdBy: isloTestKeyName}
				client.registerSandbox(isloTeardownName, isloTestResourceID)
				return client
			},
			wantDeletes: 1,
			wantProof:   isloProofTombstone,
		},
		"a claim with no recorded id still deletes by name and still needs proof": {
			legacyClaim: true,
			client: func() *fakeIsloSyncClient {
				return &fakeIsloSyncClient{getSandboxErr: errors.New("503 service unavailable")}
			},
			wantErr:     "confirm sandbox deletion by name",
			wantDeletes: 1,
			wantClaim:   true,
		},
		"a foreign resource under the claimed name is still refused": {
			client: func() *fakeIsloSyncClient {
				client := &fakeIsloSyncClient{}
				client.registerSandbox(isloTeardownName, "0195f3d2-5c1a-7c39-9c1e-000000000000")
				return client
			},
			wantErr:   "this lease does not own",
			wantClaim: true,
		},
		"a sandbox still running after the delete is still unproven": {
			client: func() *fakeIsloSyncClient {
				client := &fakeIsloSyncClient{byID: map[string]*gosdk.SandboxResponse{
					isloTestResourceID: {ID: isloTestResourceID, Name: isloTeardownName, Status: "running"},
				}}
				client.registerSandbox(isloTeardownName, isloTestResourceID)
				return client
			},
			wantErr:     "still reports status",
			wantDeletes: 1,
			wantClaim:   true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			if tc.legacyClaim {
				claimIsloLegacyLease(t, isloTeardownLeaseID)
			} else {
				claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
			}
			client := tc.client()
			var stderr bytes.Buffer
			backend := newIsloForgetMissingTeardownBackend(t, client, &stderr, true)

			err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err=%v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want %q", err, tc.wantErr)
			}
			if client.deleteCalls != tc.wantDeletes {
				t.Fatalf("delete calls=%d names=%#v, want %d", client.deleteCalls, client.deletedNames, tc.wantDeletes)
			}
			if tc.wantProof != "" && !strings.Contains(stderr.String(), "proof="+string(tc.wantProof)) {
				t.Fatalf("stderr=%q, want proof=%s reported", stderr.String(), tc.wantProof)
			}
			// The forget non-proof may never appear on a path the opt-out is
			// not meant to reach.
			if strings.Contains(stderr.String(), string(isloProofUnverifiedForgotten)) {
				t.Fatalf("stderr=%q, want no unverified-forget release on this path", stderr.String())
			}
			if tc.wantClaim {
				requireIsloClaimRetained(t, isloTeardownLeaseID)
				return
			}
			requireIsloClaimDropped(t, isloTeardownLeaseID)
		})
	}
}

// TestIsloTeardownDeletesByNameForAClaimWithNoRecordedID is the other half of
// the fail-closed rule. A claim written before the identity binding has no id to
// cross, so the name is the only handle it has ever had: a failed read must not
// strand it, and the name-only delete the pre-binding teardown always performed
// stays correct.
func TestIsloTeardownDeletesByNameForAClaimWithNoRecordedID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	isolateIsloTestHome(t)
	claimIsloLegacyLease(t, isloTeardownLeaseID)
	client := &fakeIsloSyncClient{getSandboxErr: errors.New("503 service unavailable")}
	var stderr bytes.Buffer
	backend := newIsloTeardownBackend(t, client, &stderr)

	err := backend.Stop(context.Background(), StopRequest{ID: isloTeardownLeaseID})
	if err == nil || !strings.Contains(err.Error(), "confirm sandbox deletion by name") {
		t.Fatalf("err=%v, want an unproven teardown after the confirmation read failed", err)
	}
	if client.deleteCalls != 1 || client.deletedNames[0] != isloTeardownName {
		t.Fatalf("delete calls=%d names=%#v, want the name delete still issued for a claim with no recorded id", client.deleteCalls, client.deletedNames)
	}
	if len(client.byIDCalls) != 0 {
		t.Fatalf("by-id calls=%#v, want none: the claim records no resource id", client.byIDCalls)
	}
	if !strings.Contains(stderr.String(), "could not read sandbox") {
		t.Fatalf("stderr=%q, want the failed identity read reported", stderr.String())
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
// costs them one cleanup budget in total, not one per API call. The two cases
// differ in what a starved read is allowed to lead to, which is the fail-closed
// rule itself: a claim with no recorded id still gets its name delete, while an
// id-bound claim refuses to delete a name it could not identify.
func TestIsloRunCleanupBoundsTheWholeTeardown(t *testing.T) {
	const budget = 100 * time.Millisecond
	for name, tc := range map[string]struct {
		idBound     bool
		wantErr     string
		wantDeletes int
	}{
		// Two calls starve here: the pre-flight by-name read and the DELETE. A
		// per-call budget would cost ~2x, and a call left unbounded would never
		// return at all, so the wait is what is asserted.
		"claim with no recorded id still deletes by name": {
			wantErr:     "islo delete sandbox",
			wantDeletes: 1,
		},
		// Both identity reads starve, so nothing shows the recorded name is
		// still this lease's sandbox and no delete may go out.
		"id-bound claim refuses an unidentified name": {
			idBound:     true,
			wantErr:     "refused to delete sandbox",
			wantDeletes: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			isolateIsloTestHome(t)
			withIsloCleanupTimeout(t, budget)
			if tc.idBound {
				claimIsloLeaseWithIdentity(t, isloTeardownLeaseID, "web", isloTeardownName, isloTestResourceID, isloTestClaimScope)
			} else {
				claimIsloLegacyLease(t, isloTeardownLeaseID)
			}
			client := &fakeIsloSyncClient{blockReads: true, blockDelete: true}
			client.registerSandbox(isloTeardownName, isloTestResourceID)
			backend := newIsloTeardownBackend(t, client, io.Discard)

			done := make(chan error, 1)
			go func() { done <- backend.releaseIsloLease(client, isloTeardownLeaseID, isloTeardownName) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(2 * budget):
				t.Fatalf("cleanup did not return within %s, want the whole teardown bounded by one %s budget", 2*budget, budget)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v, want %q when nothing answers", err, tc.wantErr)
			}
			if client.deleteCalls != tc.wantDeletes {
				t.Fatalf("delete calls=%d, want %d", client.deleteCalls, tc.wantDeletes)
			}
			// Counting the call is not enough. Without the reserved slice the
			// pre-flight read consumes the whole budget and the DELETE is
			// dispatched on an already cancelled context, which a real transport
			// refuses to send - the sandbox then keeps billing even though
			// deleteCalls says the delete was attempted.
			if len(client.deleteCtxErrs) != tc.wantDeletes {
				t.Fatalf("delete context observations=%d, want %d", len(client.deleteCtxErrs), tc.wantDeletes)
			}
			for _, ctxErr := range client.deleteCtxErrs {
				if ctxErr != nil {
					t.Fatalf("delete dispatched on an expired context (%v), want the reserved slice of the budget to keep it live", ctxErr)
				}
			}
			requireIsloClaimRetained(t, isloTeardownLeaseID)
		})
	}
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
