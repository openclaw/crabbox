package unikraftcloud

import (
	"context"
	"strings"
	"testing"
)

type unikraftCloudNamelessDeleteAPI struct {
	*fakeUnikraftCloudAPI
}

type unikraftCloudConflictingDeleteNameAPI struct {
	*fakeUnikraftCloudAPI
}

func (api *unikraftCloudNamelessDeleteAPI) DeleteInstance(_ context.Context, id string) (ukcInstance, error) {
	api.deletedIDs = append(api.deletedIDs, id)
	if api.deleted == nil {
		api.deleted = make(map[string]bool)
	}
	api.deleted[id] = true
	return ukcInstance{UUID: id, State: "deleted", ItemStatus: "success"}, nil
}

func (api *unikraftCloudConflictingDeleteNameAPI) DeleteInstance(_ context.Context, id string) (ukcInstance, error) {
	api.deletedIDs = append(api.deletedIDs, id)
	return ukcInstance{UUID: id, Name: "crabbox-ukc-ffffffffffff", State: "deleted", ItemStatus: "success"}, nil
}

func TestStopAcceptsExactDeleteResponseWithOmittedName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
	}
	api := &unikraftCloudNamelessDeleteAPI{fakeUnikraftCloudAPI: base}
	b := testBackend(api, nil, nil)

	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	claim := onlyTestClaim(t)
	if err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(base.deletedIDs) != 1 || base.deletedIDs[0] != testInstanceUUID {
		t.Fatalf("deleted IDs = %#v", base.deletedIDs)
	}
	if _, exists, err := readLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatalf("claim exists=%v err=%v, want removed", exists, err)
	}
}

func TestStopRetainsDeleteAttemptForConflictingResponseName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
	}
	api := &unikraftCloudConflictingDeleteNameAPI{fakeUnikraftCloudAPI: base}
	b := testBackend(api, nil, nil)

	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	claim := onlyTestClaim(t)
	err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID})
	if err == nil || !strings.Contains(err.Error(), "changed name") {
		t.Fatalf("Stop err = %v, want changed-name refusal", err)
	}
	if len(base.deletedIDs) != 1 || base.deletedIDs[0] != testInstanceUUID {
		t.Fatalf("deleted IDs = %#v", base.deletedIDs)
	}
	stored, exists, readErr := readLeaseClaimWithPresence(claim.LeaseID)
	if readErr != nil || !exists {
		t.Fatalf("claim exists=%v err=%v, want retained", exists, readErr)
	}
	if stored.Labels["state"] != ukcStateDeleteAttempt {
		t.Fatalf("claim state = %q, want %q", stored.Labels["state"], ukcStateDeleteAttempt)
	}
}
