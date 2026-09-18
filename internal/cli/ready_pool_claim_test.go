package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadyPoolClaimCLISelectsBeforeProviderProof(t *testing.T) {
	identity := testReadyPoolIdentity(t, "", "", "", "")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var input CoordinatorReadyPoolClaimInput
		if r.Method != http.MethodPost || r.URL.Path != "/v1/ready-pools/builders/claim-identity" || json.NewDecoder(r.Body).Decode(&input) != nil || !input.SelectOnly || input.LookupOnly {
			t.Errorf("unexpected selection request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(CoordinatorReadyPoolClaimResponse{Allocation: CoordinatorReadyPoolAllocation{
			Schema: "crabbox-ready-pool-allocation/v1", OperationID: input.OperationID, PoolKey: "builders", Identity: identity,
			Kind: "warm", Phase: "selected", LeaseID: "cbx_prepared", LeaseGeneration: "generation-one", SelectedAt: "2026-09-13T12:00:00Z", ReasonCode: "pool-hit",
		}})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "local-test-token")
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"pool", "claim", "builders", "--operation-id", "operation-one", "--cold-lease-id", "cbx_cold", "--identity-file", writeTestReadyPoolIdentity(t, identity), "--select-only", "--json"}); err != nil {
		t.Fatal(err)
	}
	var output CoordinatorReadyPoolClaimResponse
	if json.Unmarshal(stdout.Bytes(), &output) != nil || output.Allocation.Kind != "warm" || output.Allocation.Phase != "selected" || requests != 1 {
		t.Fatalf("selection was not retained: requests=%d output=%s", requests, stdout.String())
	}
}

func TestReadyPoolClaimFailureOnlyLooksUpDurableWarmSelection(t *testing.T) {
	identity := testReadyPoolIdentity(t, "", "", "", "")
	mutations, lookups := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input CoordinatorReadyPoolClaimInput
		_ = json.NewDecoder(r.Body).Decode(&input)
		if !input.LookupOnly {
			mutations++
			http.Error(w, "synthetic claim failure", http.StatusConflict)
			return
		}
		lookups++
		_ = json.NewEncoder(w).Encode(CoordinatorReadyPoolClaimResponse{Allocation: CoordinatorReadyPoolAllocation{
			Schema: "crabbox-ready-pool-allocation/v1", OperationID: input.OperationID, PoolKey: "builders", Identity: identity,
			Kind: "warm", Phase: "failed", LeaseID: "cbx_prepared", LeaseGeneration: "generation-one", SelectedAt: "2026-09-13T12:00:00Z", ReasonCode: "pool-claim-failed",
		}})
	}))
	defer server.Close()
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	response, err := client.ClaimReadyPoolAllocation(context.Background(), "builders", CoordinatorReadyPoolClaimInput{OperationID: "operation-one", ColdLeaseID: "cbx_cold", Identity: identity})
	if err == nil || response.Allocation.Kind != "warm" || response.Allocation.Phase != "failed" || mutations != 1 || lookups != 1 {
		t.Fatalf("claim failure lost classification or repeated mutation: response=%+v error=%v mutations=%d lookups=%d", response, err, mutations, lookups)
	}
}

func TestReadyPoolClaimResponseRequiresOriginalColdLeaseAndIdentity(t *testing.T) {
	identity := testReadyPoolIdentity(t, "", "", "", "")
	input := CoordinatorReadyPoolClaimInput{OperationID: "op", ColdLeaseID: "cbx_cold", Identity: identity}
	response := CoordinatorReadyPoolClaimResponse{Allocation: CoordinatorReadyPoolAllocation{Schema: "crabbox-ready-pool-allocation/v1", OperationID: "op", PoolKey: "builders", Identity: identity, Kind: "cold", Phase: "selected", LeaseID: "cbx_cold", SelectedAt: "2026-09-13T12:00:00Z", ReasonCode: "pool-miss"}}
	if err := validateReadyPoolClaim(response, "builders", input); err != nil {
		t.Fatal(err)
	}
	response.Allocation.LeaseID = "cbx_other"
	if validateReadyPoolClaim(response, "builders", input) == nil {
		t.Fatal("foreign cold allocation accepted")
	}
	response.Allocation.LeaseID = input.ColdLeaseID
	response.Allocation.Identity.CacheCompatibility = "other"
	if validateReadyPoolClaim(response, "builders", input) == nil {
		t.Fatal("foreign identity accepted")
	}
}
