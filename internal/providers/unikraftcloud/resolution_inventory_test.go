package unikraftcloud

import (
	"context"
	"errors"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type sequencedUnikraftCloudAPI struct {
	*fakeUnikraftCloudAPI
	listResults [][]ukcInstance
	listCalls   int
}

func (api *sequencedUnikraftCloudAPI) ListInstances(context.Context) ([]ukcInstance, error) {
	api.listCalls++
	if len(api.listResults) == 0 {
		return nil, nil
	}
	index := api.listCalls - 1
	if index >= len(api.listResults) {
		index = len(api.listResults) - 1
	}
	return append([]ukcInstance(nil), api.listResults[index]...), nil
}

func createPendingUnikraftCloudIntent(t *testing.T, b *backend, api unikraftCloudAPI) LeaseClaim {
	t.Helper()
	leaseID := newLeaseID()
	name := leaseProviderName(leaseID, "")
	intent, err := b.createIntentClaim(
		leaseID,
		"pending-intent",
		testClaimScope(t, api.BaseURL()),
		testUserUUID,
		WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}},
		createInstanceRequest{Name: name, Image: b.cfg.UnikraftCloud.Image, MemoryMB: b.cfg.UnikraftCloud.MemoryMB, Autostart: true},
	)
	if err != nil {
		t.Fatalf("create intent: %v", err)
	}
	intent, err = transitionUnikraftCloudCreateState(intent, ukcStateCreateIntent)
	if err != nil {
		t.Fatalf("arm intent: %v", err)
	}
	return intent
}

func TestListReconcilesPendingIntentAgainstOneInventorySnapshot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}
	api := &sequencedUnikraftCloudAPI{fakeUnikraftCloudAPI: base}
	b := testBackend(api, nil, nil)
	intent := createPendingUnikraftCloudIntent(t, b, api)
	api.listResults = [][]ukcInstance{
		nil,
		{{UUID: testInstanceUUID, Name: intent.Labels[ukcLabelResourceName], State: "running"}},
	}

	servers, err := b.List(context.Background(), ListRequest{All: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if api.listCalls != 1 {
		t.Fatalf("ListInstances calls = %d, want one consistent snapshot", api.listCalls)
	}
	if len(servers) != 1 || servers[0].CloudID != "" || servers[0].Status != ukcStateCreateIntent {
		t.Fatalf("servers = %#v, want pending intent only", servers)
	}
	stored := onlyTestClaim(t)
	if stored.CloudID != "" || stored.Labels["state"] != ukcStateCreateIntent {
		t.Fatalf("stored claim = %#v, want unchanged intent", stored)
	}
}

func TestStatusExactCloudIDPrecedesUUIDShapedSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	apiA := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud", createResult: ukcInstance{UUID: testInstanceUUID, State: "running"}}
	bA := testBackend(apiA, nil, nil)
	if err := bA.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "a"}, RequestedSlug: "owner-a"}); err != nil {
		t.Fatalf("Warmup A: %v", err)
	}

	const otherUUID = "66666666-7777-8888-9999-000000000000"
	apiB := &fakeUnikraftCloudAPI{baseURL: apiA.BaseURL(), createResult: ukcInstance{UUID: otherUUID, State: "running"}}
	bB := testBackend(apiB, nil, nil)
	if err := bB.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "b"}, RequestedSlug: testInstanceUUID}); err != nil {
		t.Fatalf("Warmup B: %v", err)
	}

	claims, err := listUnikraftCloudLeaseClaims()
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	wantLeaseID := ""
	for _, claim := range claims {
		if claim.CloudID == testInstanceUUID {
			wantLeaseID = claim.LeaseID
		}
	}
	view, err := bA.Status(context.Background(), StatusRequest{ID: testInstanceUUID})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if wantLeaseID == "" || view.ID != wantLeaseID || view.ServerID != testInstanceUUID || view.Slug != "owner-a" {
		t.Fatalf("view = %#v, want exact CloudID owner %s", view, wantLeaseID)
	}
}

func TestStatusRawUnclaimedUUIDStillWorks(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:    "https://api.fra.unikraft.cloud",
		getResults: []ukcInstance{{UUID: testInstanceUUID, Name: "unmanaged", State: "running"}},
	}
	b := testBackend(api, nil, nil)

	view, err := b.Status(context.Background(), StatusRequest{ID: testInstanceUUID})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if view.ID != testInstanceUUID || view.ServerID != testInstanceUUID || view.Slug != "" || view.Labels["lease"] != "" {
		t.Fatalf("view = %#v, want unmanaged raw UUID", view)
	}
}

func TestStatusDoesNotRawFallbackForCorruptExactClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud", createResult: ukcInstance{UUID: testInstanceUUID, State: "running"}}
	b := testBackend(api, nil, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	claim := onlyTestClaim(t)
	labels := cloneLabels(claim.Labels)
	labels[ukcLabelAccountUUID] = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels); err != nil {
		t.Fatalf("corrupt claim: %v", err)
	}

	_, err := b.Status(context.Background(), StatusRequest{ID: claim.LeaseID})
	if err == nil || !strings.Contains(err.Error(), "account identity") {
		t.Fatalf("Status err = %v, want exact claim corruption", err)
	}
	if api.getCalls != 0 {
		t.Fatalf("GetInstance calls = %d, want no raw fallback", api.getCalls)
	}
}

func TestStopTreatsLeaseShapedIdentifierAsExactOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud", createResult: ukcInstance{UUID: testInstanceUUID, State: "running"}}
	b := testBackend(api, nil, nil)
	const staleLeaseID = "ukc_stale-missing"
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}, RequestedSlug: staleLeaseID}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}

	err := b.Stop(context.Background(), StopRequest{ID: staleLeaseID})
	if err == nil || !strings.Contains(err.Error(), "not claimed") {
		t.Fatalf("Stop err = %v, want exact missing lease", err)
	}
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 4 {
		t.Fatalf("Stop err = %#v, want exit 4", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("deletedIDs = %#v, want no slug fallback mutation", api.deletedIDs)
	}
}

func TestWarmupRejectsMalformedPreflightInventoryBeforeCreate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:    "https://api.fra.unikraft.cloud",
		listResult: []ukcInstance{{UUID: "not-a-uuid", Name: "unrelated", State: "running"}},
	}
	b := testBackend(api, nil, nil)

	err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}})
	if err == nil || !strings.Contains(err.Error(), "invalid instance UUID") {
		t.Fatalf("Warmup err = %v, want malformed inventory", err)
	}
	if len(api.created) != 0 {
		t.Fatalf("created = %#v, want no POST", api.created)
	}
	if claims, listErr := listUnikraftCloudLeaseClaims(); listErr != nil || len(claims) != 0 {
		t.Fatalf("claims = %#v err=%v, want preflight removed", claims, listErr)
	}
}

func TestStopRetainsClaimWhenAbsenceInventoryIsMalformed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud", createResult: ukcInstance{UUID: testInstanceUUID, State: "running"}}
	b := testBackend(api, nil, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	claim := onlyTestClaim(t)
	api.getErr = notFoundErr()
	api.listResult = []ukcInstance{{UUID: "not-a-uuid", Name: "unrelated", State: "running"}}

	err := b.Stop(context.Background(), StopRequest{ID: claim.LeaseID})
	if err == nil || !strings.Contains(err.Error(), "invalid instance UUID") {
		t.Fatalf("Stop err = %v, want malformed absence inventory", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("deletedIDs = %#v, want no delete after ambiguous GET", api.deletedIDs)
	}
	if _, exists, readErr := readLeaseClaimWithPresence(claim.LeaseID); readErr != nil || !exists {
		t.Fatalf("claim exists=%v err=%v, want retained", exists, readErr)
	}
}
