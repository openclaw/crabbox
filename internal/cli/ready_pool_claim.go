package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
)

type CoordinatorReadyPoolAllocation struct {
	Schema          string                         `json:"schema"`
	OperationID     string                         `json:"operationID"`
	PoolKey         string                         `json:"poolKey"`
	Identity        CoordinatorReadyPoolIdentityV1 `json:"identity"`
	Kind            string                         `json:"kind"`
	Phase           string                         `json:"phase"`
	LeaseID         string                         `json:"leaseID"`
	LeaseGeneration string                         `json:"leaseGeneration,omitempty"`
	SelectedAt      string                         `json:"selectedAt"`
	SettledAt       string                         `json:"settledAt,omitempty"`
	ReasonCode      string                         `json:"reasonCode"`
}

type CoordinatorReadyPoolClaimResponse struct {
	Allocation CoordinatorReadyPoolAllocation `json:"allocation"`
	Entry      *CoordinatorReadyPoolEntry     `json:"entry,omitempty"`
	Lease      *CoordinatorLease              `json:"lease,omitempty"`
}

type CoordinatorReadyPoolClaimInput struct {
	OperationID string                         `json:"operationID"`
	ColdLeaseID string                         `json:"coldLeaseID"`
	Identity    CoordinatorReadyPoolIdentityV1 `json:"identity"`
	LookupOnly  bool                           `json:"lookupOnly,omitempty"`
	SelectOnly  bool                           `json:"selectOnly,omitempty"`
}

var readyPoolOperationPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var readyPoolLeasePattern = regexp.MustCompile(`^cbx_[A-Za-z0-9_-]{1,128}$`)

func (c *CoordinatorClient) ClaimReadyPoolAllocation(ctx context.Context, key string, input CoordinatorReadyPoolClaimInput) (CoordinatorReadyPoolClaimResponse, error) {
	var response CoordinatorReadyPoolClaimResponse
	err := c.doTypedReadyPool(ctx, http.MethodPost, key, "claim-identity", input, &response)
	if err != nil {
		// A timed-out request may have selected a runner. Read its durable decision;
		// never issue another mutating claim or relabel a warm failure as cold.
		if !input.LookupOnly {
			lookup := input
			lookup.LookupOnly, lookup.SelectOnly = true, false
			_ = c.doTypedReadyPool(ctx, http.MethodPost, key, "claim-identity", lookup, &response)
		}
		return response, err
	}
	return response, validateReadyPoolClaim(response, key, input)
}

func validateReadyPoolClaim(response CoordinatorReadyPoolClaimResponse, key string, input CoordinatorReadyPoolClaimInput) error {
	allocation := response.Allocation
	invalid := func() error { return Exit(7, "coordinator returned an invalid or fenced ready-pool allocation") }
	if allocation.Schema != "crabbox-ready-pool-allocation/v1" || allocation.OperationID != input.OperationID ||
		allocation.PoolKey != key || allocation.Identity != input.Identity || !readyPoolLeasePattern.MatchString(allocation.LeaseID) || allocation.SelectedAt == "" {
		return invalid()
	}
	if allocation.Kind == "cold" {
		if allocation.LeaseID != input.ColdLeaseID || allocation.Phase != "selected" ||
			(allocation.ReasonCode != "pool-miss" && !(input.LookupOnly && allocation.ReasonCode == "pool-unselected")) {
			return invalid()
		}
		return nil
	}
	if allocation.Kind != "warm" || allocation.LeaseGeneration == "" {
		return invalid()
	}
	if input.LookupOnly {
		switch allocation.Phase {
		case "selected", "claiming", "ready", "failed":
			return nil
		}
		return invalid()
	}
	if input.SelectOnly && allocation.Phase == "selected" && allocation.ReasonCode == "pool-hit" {
		return nil
	}
	if allocation.Phase != "ready" || allocation.ReasonCode != "pool-hit" || response.Lease == nil || response.Entry == nil ||
		response.Lease.ID != allocation.LeaseID || response.Lease.State != "active" || response.Entry.LeaseID != allocation.LeaseID ||
		response.Entry.Key != key || response.Entry.State != "busy" || response.Entry.Identity == nil || *response.Entry.Identity != input.Identity {
		return invalid()
	}
	return readyPoolIdentityMatchesLease(input.Identity, *response.Lease)
}

func (a App) readyPoolClaim(ctx context.Context, args []string) error {
	fs := newFlagSet("pool claim", a.Stderr)
	operation := fs.String("operation-id", "", "stable allocation operation id")
	coldID := fs.String("cold-lease-id", "", "deterministic lease id to use on an actual pool miss")
	identityFile := fs.String("identity-file", "", "operator-owned typed pool identity file")
	lookup := fs.Bool("lookup-only", false, "read the selection without reserving inventory")
	selectOnly := fs.Bool("select-only", false, "commit warm/cold selection before proving the clean claim")
	jsonOut := fs.Bool("json", false, "print allocation JSON")
	args, key := extractFirstPositionalArg(args, map[string]bool{"operation-id": true, "cold-lease-id": true, "identity-file": true})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if key == "" || !readyPoolOperationPattern.MatchString(*operation) || !readyPoolLeasePattern.MatchString(*coldID) || (*lookup && *selectOnly) {
		return Exit(2, "pool claim requires a key, --operation-id, --cold-lease-id and --identity-file; select-only and lookup-only are exclusive")
	}
	identity, err := loadReadyPoolIdentity(*identityFile)
	if err != nil {
		return err
	}
	coord, err := readyPoolCoordinator()
	if err != nil {
		return err
	}
	input := CoordinatorReadyPoolClaimInput{OperationID: *operation, ColdLeaseID: *coldID, Identity: identity, LookupOnly: *lookup, SelectOnly: *selectOnly}
	response, err := coord.ClaimReadyPoolAllocation(ctx, key, input)
	if err != nil {
		readInput := input
		readInput.LookupOnly, readInput.SelectOnly = true, false
		if *jsonOut && validateReadyPoolClaim(response, key, readInput) == nil && response.Allocation.ReasonCode != "pool-unselected" {
			// Allocation fields contain no claim token or provider diagnostics.
			_ = json.NewEncoder(a.Stdout).Encode(map[string]any{"allocation": response.Allocation, "error": "pool_claim_failed"})
		}
		return Exit(7, "ready-pool claim failed; inspect its durable allocation before cleanup")
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(response)
	}
	fmt.Fprintf(a.Stdout, "allocation=%s state=%s lease=%s reason=%s\n", response.Allocation.Kind, response.Allocation.Phase, response.Allocation.LeaseID, response.Allocation.ReasonCode)
	return nil
}
