package cua

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestCUALeaseIDUsesProviderPrefix(t *testing.T) {
	leaseID := newCUALeaseID()
	if !strings.HasPrefix(leaseID, leasePrefix) {
		t.Fatalf("leaseID=%q missing %q", leaseID, leasePrefix)
	}
}

func TestCUAScopeNormalizesAPIURL(t *testing.T) {
	t.Setenv("CRABBOX_CUA_API_KEY", "account-key-a")
	a := testConfig()
	a.Cua.APIURL = "https://API.CUA.EXAMPLE:443/v1/"
	b := testConfig()
	b.Cua.APIURL = "https://api.cua.example/v1"
	scopeA, err := cuaScope(a)
	if err != nil {
		t.Fatal(err)
	}
	scopeB, err := cuaScope(b)
	if err != nil {
		t.Fatal(err)
	}
	if scopeA != scopeB || !strings.HasPrefix(scopeA, scopePrefix) {
		t.Fatalf("scopes=%q %q", scopeA, scopeB)
	}
	t.Setenv("CRABBOX_CUA_API_KEY", "account-key-b")
	scopeC, err := cuaScope(b)
	if err != nil {
		t.Fatal(err)
	}
	if scopeC == scopeB {
		t.Fatalf("credential-selected accounts must not share claim scope: %q", scopeC)
	}
	if strings.Contains(scopeC, "account-key-b") {
		t.Fatalf("claim scope leaked credential material: %q", scopeC)
	}
}

func TestResolveClaimFiltersProviderAndScope(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := testConfig()
	scope, err := cuaScope(cfg)
	if err != nil {
		t.Fatal(err)
	}
	writeClaim(t, core.LeaseClaim{
		LeaseID:       "cuabx_111111111111",
		Slug:          "demo",
		Provider:      providerName,
		CloudID:       "crabbox-cua-demo-111111",
		ProviderScope: scope,
		Labels:        claimLabels(cfg, "crabbox-cua-demo-111111", "2026-07-03T12:00:00Z", true),
	})
	claim, ok, err := resolveCUALeaseClaim("demo", cfg)
	if err != nil {
		t.Fatalf("resolve claim: %v", err)
	}
	if !ok || claim.LeaseID != "cuabx_111111111111" || !claimIsMissing(claim) {
		t.Fatalf("claim=%#v ok=%v", claim, ok)
	}
	other := cfg
	other.Cua.APIURL = "https://other.cua.example"
	if _, _, err := resolveCUALeaseClaim("demo", other); err == nil {
		t.Fatal("expected scope mismatch rejection")
	}
}

func TestValidateSandboxOwnershipFailsClosed(t *testing.T) {
	cfg := testConfig()
	scope, err := cuaScope(cfg)
	if err != nil {
		t.Fatal(err)
	}
	claim := core.LeaseClaim{
		LeaseID:       "cuabx_222222222222",
		Provider:      providerName,
		ProviderScope: scope,
		CloudID:       "crabbox-cua-demo-222222",
		Labels:        map[string]string{labelCreatedAt: "2026-07-03T12:00:00Z"},
	}
	metadata := map[string]string{"createdAt": "2026-07-03T12:00:00Z"}
	if err := validateSandboxOwnership(claim, bridgeSandboxSummary{Name: "crabbox-cua-demo-222222", Metadata: metadata}, scope); err != nil {
		t.Fatalf("expected ownership to pass: %v", err)
	}
	if err := validateSandboxOwnership(claim, bridgeSandboxSummary{Name: "someone-else", Metadata: metadata}, scope); err == nil {
		t.Fatal("expected name mismatch rejection")
	}
	claim.CloudID = "provider-assigned-name"
	if err := validateSandboxOwnership(claim, bridgeSandboxSummary{Name: "provider-assigned-name", ID: "different-id", Metadata: metadata}, scope); err == nil {
		t.Fatal("expected conflicting identity rejection")
	}
	if err := validateSandboxOwnership(claim, bridgeSandboxSummary{}, scope); err == nil {
		t.Fatal("expected missing identity rejection")
	}
	if err := validateSandboxOwnership(claim, bridgeSandboxSummary{Name: "provider-assigned-name", Metadata: map[string]string{"createdAt": "different"}}, scope); err == nil {
		t.Fatal("expected creation timestamp mismatch rejection")
	}
}

func writeClaim(t *testing.T, claim core.LeaseClaim) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, claim.LeaseID+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
