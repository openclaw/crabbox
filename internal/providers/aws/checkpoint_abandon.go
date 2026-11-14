package aws

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// NativeCheckpointAbandonScope validates persisted cleanup coordinates without
// deriving authority from the caller's current account or region defaults.
func (*awsLeaseBackend) NativeCheckpointAbandonScope(claim *core.LeaseClaim, capture core.NativeCheckpointCapture, scope core.NativeCheckpointImage) (core.NativeCheckpointImage, error) {
	account, region := scope.AccountID, scope.Region
	if claim != nil {
		account, region = strings.TrimSpace(claim.Labels["aws_account_id"]), strings.TrimSpace(claim.Labels["aws_region"])
	}
	// Ordinary legacy claims may have no provider scope. Their exact bound
	// claim labels attest the account; fixed intents still require account scope.
	if len(account) != 12 || strings.Trim(account, "0123456789") != "" || region == "" || (capture.SourceScope != "account:"+account && !(capture.SourceScope == "" && capture.SourceIntent == "")) {
		return scope, core.Exit(2, "AWS checkpoint abandonment source account or region is invalid; retain operation")
	}
	if scope.AccountID != "" && scope.AccountID != account || scope.Region != "" && scope.Region != region {
		return scope, core.Exit(2, "AWS checkpoint abandonment cleanup scope conflicts with its retained source receipt")
	}
	scope.AccountID, scope.Region = account, region
	return scope, nil
}
