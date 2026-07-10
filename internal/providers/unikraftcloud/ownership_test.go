package unikraftcloud

import (
	"context"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func duplicateBoundUnikraftCloudClaims(t *testing.T) (*backend, *fakeUnikraftCloudAPI, LeaseClaim, LeaseClaim) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := &fakeUnikraftCloudAPI{
		baseURL:      "https://api.fra.unikraft.cloud",
		createResult: ukcInstance{UUID: testInstanceUUID, State: "running"},
	}
	b := testBackend(api, nil, nil)
	if err := b.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}}); err != nil {
		t.Fatalf("Warmup: %v", err)
	}
	first := onlyTestClaim(t)

	secondLeaseID := newLeaseID()
	secondName := leaseProviderName(secondLeaseID, "")
	createReq := createInstanceRequest{
		Name:      secondName,
		Image:     b.cfg.UnikraftCloud.Image,
		MemoryMB:  b.cfg.UnikraftCloud.MemoryMB,
		Autostart: true,
	}
	secondIntent, err := b.createIntentClaim(
		secondLeaseID,
		"duplicate-owner",
		testClaimScope(t, api.BaseURL()),
		testUserUUID,
		WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}, Keep: true},
		createReq,
	)
	if err != nil {
		t.Fatalf("create second intent: %v", err)
	}
	second, err := b.publishReadyClaim(secondIntent, ukcInstance{UUID: testInstanceUUID, Name: secondName, State: "running"})
	if err != nil {
		t.Fatalf("publish second claim: %v", err)
	}
	return b, api, first, second
}

func TestStopRejectsDuplicateInstanceClaimsBeforeMutation(t *testing.T) {
	b, api, first, _ := duplicateBoundUnikraftCloudClaims(t)

	err := b.Stop(context.Background(), StopRequest{ID: first.LeaseID})
	if err == nil || !strings.Contains(err.Error(), "claimed by both") {
		t.Fatalf("Stop err = %v, want duplicate ownership error", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("deletedIDs = %#v, want no mutation", api.deletedIDs)
	}
}

func TestCleanupRejectsDuplicateInstanceClaimsBeforeMutation(t *testing.T) {
	b, api, first, _ := duplicateBoundUnikraftCloudClaims(t)
	labels := cloneLabels(first.Labels)
	labels["keep"] = "false"
	labels["expires_at"] = "1"
	if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(first.LeaseID, first, labels); err != nil {
		t.Fatalf("expire first claim: %v", err)
	}

	err := b.Cleanup(context.Background(), CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "claimed by both") {
		t.Fatalf("Cleanup err = %v, want duplicate ownership error", err)
	}
	if len(api.deletedIDs) != 0 {
		t.Fatalf("deletedIDs = %#v, want no mutation", api.deletedIDs)
	}
}

func TestStatusRejectsDuplicateInstanceClaimAmbiguity(t *testing.T) {
	b, api, _, _ := duplicateBoundUnikraftCloudClaims(t)

	_, err := b.Status(context.Background(), StatusRequest{ID: testInstanceUUID})
	if err == nil || !strings.Contains(err.Error(), "claimed by multiple") {
		t.Fatalf("Status err = %v, want duplicate CloudID ambiguity", err)
	}
	if api.getCalls != 0 {
		t.Fatalf("GetInstance calls = %d, want no raw fallback", api.getCalls)
	}
}

func TestOwnershipPreflightCanonicalizesInstanceUUIDCase(t *testing.T) {
	_, _, first, second := duplicateBoundUnikraftCloudClaims(t)
	second.CloudID = strings.ToUpper(second.CloudID)
	second.Labels[ukcLabelInstanceUUID] = second.CloudID

	err := preflightUnikraftCloudClaimOwnership([]LeaseClaim{first, second}, first.ProviderScope)
	if err == nil || !strings.Contains(err.Error(), "claimed by both") {
		t.Fatalf("preflight err = %v, want case-insensitive duplicate UUID rejection", err)
	}
}

func TestOwnershipPreflightRejectsPendingResourceNameCollision(t *testing.T) {
	_, _, first, second := duplicateBoundUnikraftCloudClaims(t)
	second.CloudID = ""
	second.Labels[ukcLabelInstanceUUID] = ""
	second.Labels[ukcLabelResourceName] = first.Labels[ukcLabelResourceName]

	err := preflightUnikraftCloudClaimOwnership([]LeaseClaim{first, second}, first.ProviderScope)
	if err == nil || !strings.Contains(err.Error(), "recovery resource name") {
		t.Fatalf("preflight err = %v, want corrupt resource-name rejection", err)
	}
}
