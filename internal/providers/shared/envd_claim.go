package shared

import (
	"context"

	core "github.com/openclaw/crabbox/internal/cli"
)

// DeleteClaimedEnvdSandbox keeps the original claim fenced through live ownership
// validation, deletion, and claim removal. Adapters must first establish exact
// endpoint/resource binding; they retain ownership and error interpretation.
func DeleteClaimedEnvdSandbox(ctx context.Context, client EnvdSandboxAPI, leaseID, sandboxID string, claim core.LeaseClaim, validate func(EnvdSandbox) error, isNotFound func(error) bool, operationError func(string, error) error) error {
	return core.RemoveLeaseClaimIfUnchangedAfter(leaseID, claim, func() error {
		sandbox, err := client.GetSandbox(ctx, sandboxID)
		if err != nil {
			if isNotFound(err) {
				return nil
			}
			return operationError("get sandbox before delete", err)
		}
		if err := validate(sandbox); err != nil {
			return err
		}
		if err := client.DeleteSandbox(ctx, sandboxID); err != nil {
			if isNotFound(err) {
				return nil
			}
			return operationError("delete sandbox", err)
		}
		return nil
	})
}
