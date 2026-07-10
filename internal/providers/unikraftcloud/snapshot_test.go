package unikraftcloud

import (
	"strings"
	"testing"
)

func TestVerifyUnikraftCloudClaimSnapshotRejectsRebind(t *testing.T) {
	snapshot := LeaseClaim{
		LeaseID:       "ukc_aaaaaaaaaaaa",
		Slug:          "snapshot",
		Provider:      providerName,
		CloudID:       testInstanceUUID,
		ProviderScope: "endpoint:https://api.fra.unikraft.cloud|account:" + testUserUUID,
		Labels: map[string]string{
			"state":              ukcStateReady,
			ukcLabelInstanceUUID: testInstanceUUID,
			ukcLabelResourceName: "crabbox-ukc-aaaaaaaaaaaa",
			ukcLabelAccountUUID:  testUserUUID,
			ukcLabelRequestHash:  strings.Repeat("a", 64),
			"lease":              "ukc_aaaaaaaaaaaa",
			"provider":           providerName,
			"slug":               "snapshot",
		},
	}
	current := snapshot
	current.Labels = cloneLabels(snapshot.Labels)
	current.CloudID = "66666666-7777-8888-9999-000000000000"
	current.Labels[ukcLabelInstanceUUID] = current.CloudID

	if err := verifyUnikraftCloudClaimSnapshot(snapshot, current); err == nil || !strings.Contains(err.Error(), "changed while waiting") {
		t.Fatalf("snapshot verification err = %v", err)
	}
}

func TestUnikraftCloudClaimScopeRejectsInjectedAccountIdentity(t *testing.T) {
	_, err := unikraftCloudClaimScope(
		"https://api.fra.unikraft.cloud",
		testUserUUID+"|account:bbbbbbbb-cccc-dddd-eeee-ffffffffffff",
	)
	if err == nil || !strings.Contains(err.Error(), "account identity is unavailable") {
		t.Fatalf("claim scope err = %v", err)
	}
}
