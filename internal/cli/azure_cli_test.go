package cli

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAzAccountInfoParsesValidJSON(t *testing.T) {
	t.Parallel()
	input := `{"id":"00000000-0000-0000-0000-000000000001","tenantId":"00000000-0000-0000-0000-000000000002","name":"My Subscription"}`
	var info azAccountInfo
	if err := json.Unmarshal([]byte(input), &info); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.ID != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("got ID=%q", info.ID)
	}
	if info.TenantID != "00000000-0000-0000-0000-000000000002" {
		t.Fatalf("got TenantID=%q", info.TenantID)
	}
	if info.Name != "My Subscription" {
		t.Fatalf("got Name=%q", info.Name)
	}
}

func TestAzAccountInfoRejectsEmptySubscription(t *testing.T) {
	t.Parallel()
	input := `{"id":"","tenantId":"tenant","name":"empty"}`
	var info azAccountInfo
	if err := json.Unmarshal([]byte(input), &info); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.ID != "" {
		t.Fatalf("expected empty ID")
	}
}

func TestAzAccountShowRequiresAzOnPath(t *testing.T) {
	t.Setenv("PATH", "")
	_, err := azAccountShow(context.Background(), "")
	if err == nil {
		t.Fatal("expected error when az is not on PATH")
	}
	if got := err.Error(); got == "" {
		t.Fatal("expected non-empty error message")
	}
}
