package azure

import core "github.com/openclaw/crabbox/internal/cli"

type azureFixedShortagePending struct {
	LeaseID, AttemptName, AttemptNonce, ProviderCode string
	Cause                                            error
}

func (r *azureFixedShortagePending) Error() string { return r.Cause.Error() }
func (r *azureFixedShortagePending) Unwrap() error { return r.Cause }

func (r *azureFixedShortagePending) FixedRejectionSettled() error {
	return &core.FixedAllocationResult{
		Schema: "crabbox.fixed-allocation-result.v1", Capability: "azure-fixed-vm-capacity-v1",
		Provider: "azure", LeaseID: r.LeaseID, AttemptName: r.AttemptName, AttemptNonce: r.AttemptNonce,
		Category: "capacity_shortage", ProviderCode: r.ProviderCode,
		Allocation: "settled_nonallocation", Companions: "settled", Cause: r.Cause,
	}
}
