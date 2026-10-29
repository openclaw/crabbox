//go:build !crabbox_claim_fence_probe

package cli

// claimFenceProbe is the claim-fence injection point used by the live
// authority harness. Only builds tagged crabbox_claim_fence_probe compile the
// probe (claim_fence_probe.go); every other build returns the action unchanged
// and never reads the probe environment.
func claimFenceProbe(_ string, action func() error) (func() error, func(error) error) {
	return action, passClaimFenceResult
}

func passClaimFenceResult(err error) error { return err }
