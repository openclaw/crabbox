package unikraftcloud

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type unikraftCloudProvenanceAPI struct {
	*fakeUnikraftCloudAPI
	listInstances func(context.Context) ([]ukcInstance, error)
}

type unikraftCloudEventuallyVisibleCreateAPI struct {
	*fakeUnikraftCloudAPI
	getCalls int
}

func (api *unikraftCloudEventuallyVisibleCreateAPI) GetInstance(_ context.Context, id string) (ukcInstance, error) {
	api.getCalls++
	if api.getCalls < 3 || len(api.created) == 0 {
		return ukcInstance{}, notFoundErr()
	}
	return ukcInstance{UUID: testInstanceUUID, Name: api.created[0].Name, State: "running"}, nil
}

func (api *unikraftCloudEventuallyVisibleCreateAPI) ListInstances(_ context.Context) ([]ukcInstance, error) {
	if api.getCalls < 3 || len(api.created) == 0 {
		return nil, nil
	}
	return []ukcInstance{{UUID: testInstanceUUID, Name: api.created[0].Name, State: "running"}}, nil
}

func (api *unikraftCloudProvenanceAPI) ListInstances(ctx context.Context) ([]ukcInstance, error) {
	return api.listInstances(ctx)
}

func TestWarmupRefusesPreexistingGeneratedNameBeforeCreate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}
	api := &unikraftCloudProvenanceAPI{fakeUnikraftCloudAPI: base}
	api.listInstances = func(context.Context) ([]ukcInstance, error) {
		claims, err := listUnikraftCloudLeaseClaims()
		if err != nil || len(claims) != 1 {
			t.Fatalf("preflight claims = %#v err=%v, want one", claims, err)
		}
		claim := claims[0]
		if claim.Labels["state"] != ukcStateCreatePreflight || claim.CloudID != "" {
			t.Fatalf("preflight claim = %#v", claim)
		}
		return []ukcInstance{{
			UUID:  "66666666-7777-8888-9999-000000000000",
			Name:  claim.Labels[ukcLabelResourceName],
			State: "running",
		}}, nil
	}
	b := testBackend(api, nil, nil)

	err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}})
	if err == nil || !strings.Contains(err.Error(), "already exists before create") {
		t.Fatalf("Warmup err = %v, want ownership conflict", err)
	}
	if len(base.created) != 0 || len(base.deletedIDs) != 0 {
		t.Fatalf("provider mutations = created %#v deleted %#v, want none", base.created, base.deletedIDs)
	}
	if claims, listErr := listUnikraftCloudLeaseClaims(); listErr != nil || len(claims) != 0 {
		t.Fatalf("claims = %#v err=%v, want preflight removed", claims, listErr)
	}
}

func TestWarmupRefusesMalformedPreflightInventoryBeforeCreate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}
	api := &unikraftCloudProvenanceAPI{fakeUnikraftCloudAPI: base}
	api.listInstances = func(context.Context) ([]ukcInstance, error) {
		return []ukcInstance{{UUID: "not-a-uuid", Name: "unrelated", State: "running"}}, nil
	}
	b := testBackend(api, nil, nil)

	err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}})
	if err == nil || !strings.Contains(err.Error(), "invalid instance UUID") {
		t.Fatalf("Warmup err = %v, want malformed inventory rejection", err)
	}
	if len(base.created) != 0 || len(base.deletedIDs) != 0 {
		t.Fatalf("provider mutations = created %#v deleted %#v, want none", base.created, base.deletedIDs)
	}
	if claims, listErr := listUnikraftCloudLeaseClaims(); listErr != nil || len(claims) != 0 {
		t.Fatalf("claims = %#v err=%v, want preflight removed", claims, listErr)
	}
}

func TestWarmupNeverAdoptsExactNameAfterDefiniteCreateRejection(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := &fakeUnikraftCloudAPI{
		baseURL:   "https://api.fra.unikraft.cloud",
		createErr: &unikraftCloudAPIError{StatusCode: http.StatusConflict, Message: "name already exists"},
	}
	api := &unikraftCloudProvenanceAPI{fakeUnikraftCloudAPI: base}
	api.listInstances = func(context.Context) ([]ukcInstance, error) {
		if len(base.created) == 0 {
			return nil, nil
		}
		return []ukcInstance{{
			UUID:  "66666666-7777-8888-9999-000000000000",
			Name:  base.created[0].Name,
			State: "running",
		}}, nil
	}
	b := testBackend(api, nil, nil)

	err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}})
	if err == nil || !strings.Contains(err.Error(), "non-adoptable recovery claim") {
		t.Fatalf("Warmup err = %v, want retained non-adoptable conflict", err)
	}
	if len(base.created) != 1 || len(base.deletedIDs) != 0 {
		t.Fatalf("provider mutations = created %#v deleted %#v", base.created, base.deletedIDs)
	}
	conflict := onlyTestClaim(t)
	if conflict.CloudID != "" || conflict.Labels["state"] != ukcStateCreateConflict {
		t.Fatalf("conflict claim = %#v", conflict)
	}

	if _, listErr := b.List(context.Background(), ListRequest{}); listErr != nil {
		t.Fatalf("List conflict claim: %v", listErr)
	}
	stillConflict := onlyTestClaim(t)
	if stillConflict.CloudID != "" || stillConflict.Labels["state"] != ukcStateCreateConflict {
		t.Fatalf("List adopted rejected create: %#v", stillConflict)
	}
	if stopErr := b.Stop(context.Background(), StopRequest{ID: conflict.LeaseID}); stopErr == nil {
		t.Fatal("Stop adopted or deleted a rejected create conflict")
	}
	if len(base.deletedIDs) != 0 {
		t.Fatalf("deleted IDs = %#v, want none", base.deletedIDs)
	}
}

func TestStopRetainsAmbiguousCreateThatAppearsDuringAbsenceGrace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := &fakeUnikraftCloudAPI{
		baseURL:   "https://api.fra.unikraft.cloud",
		createErr: errors.New("connection reset after create"),
	}
	api := &unikraftCloudEventuallyVisibleCreateAPI{fakeUnikraftCloudAPI: base}
	b := testBackend(api, nil, nil)

	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err == nil {
		t.Fatal("Warmup succeeded, want ambiguous create error")
	}
	intent := onlyTestClaim(t)
	if intent.CloudID != "" || intent.Labels["state"] != ukcStateCreateIntent {
		t.Fatalf("intent = %#v", intent)
	}
	err := b.Stop(context.Background(), StopRequest{ID: intent.LeaseID})
	if err == nil || !strings.Contains(err.Error(), "became visible") {
		t.Fatalf("Stop err = %v, want eventual-visibility retention", err)
	}
	retained := onlyTestClaim(t)
	if retained.CloudID != "" || retained.Labels["state"] != ukcStateCreateIntent {
		t.Fatalf("retained claim = %#v", retained)
	}
	if len(base.deletedIDs) != 0 {
		t.Fatalf("deleted IDs = %#v, want none", base.deletedIDs)
	}
}
