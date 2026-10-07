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
