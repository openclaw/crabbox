package unikraftcloud

import (
	"context"
	"testing"
)

type unikraftCloudNamelessDeleteAPI struct {
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
