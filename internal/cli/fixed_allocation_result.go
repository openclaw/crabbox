package cli

// FixedAllocationResult is emitted only after the canonical fixed rejection
// finalizer has removed the claim. Consumers must still verify the schema and
// capability before treating it as a capacity-wait input.
type FixedAllocationResult struct {
	Schema       string `json:"schema"`
	Capability   string `json:"capability"`
	Provider     string `json:"provider"`
	LeaseID      string `json:"leaseId"`
	AttemptName  string `json:"attemptName"`
	AttemptNonce string `json:"attemptNonce"`
	Category     string `json:"category"`
	ProviderCode string `json:"providerCode"`
	Allocation   string `json:"allocation"`
	Companions   string `json:"companions"`
	Cause        error  `json:"-"`
}

func (r *FixedAllocationResult) Error() string { return r.Cause.Error() }
func (r *FixedAllocationResult) Unwrap() error { return r.Cause }

type AzureFixedShortagePending struct {
	LeaseID, AttemptName, AttemptNonce, ProviderCode string
	Cause                                            error
}

func (r *AzureFixedShortagePending) Error() string { return r.Cause.Error() }
func (r *AzureFixedShortagePending) Unwrap() error { return r.Cause }

func (r *AzureFixedShortagePending) FixedRejectionSettled() error {
	return &FixedAllocationResult{
		Schema: "crabbox.fixed-allocation-result.v1", Capability: "azure-fixed-vm-capacity-v1",
		Provider: "azure", LeaseID: r.LeaseID, AttemptName: r.AttemptName, AttemptNonce: r.AttemptNonce,
		Category: "capacity_shortage", ProviderCode: r.ProviderCode,
		Allocation: "settled_nonallocation", Companions: "settled", Cause: r.Cause,
	}
}
