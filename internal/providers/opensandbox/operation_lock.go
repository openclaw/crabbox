package opensandbox

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/openclaw/crabbox/internal/providers/shared"
)

func lockOpenSandboxLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	validLeaseID := strings.HasPrefix(leaseID, leasePrefix) && strings.TrimPrefix(leaseID, leasePrefix) != ""
	validRecoveryID := strings.HasPrefix(leaseID, recoveryPrefix) && strings.TrimPrefix(leaseID, recoveryPrefix) != ""
	if (!validLeaseID && !validRecoveryID) || filepath.Base(leaseID) != leaseID || leaseID == "." {
		return nil, exit(2, "invalid opensandbox lease id %q", leaseID)
	}
	return shared.LockLeaseOperation(ctx, "opensandbox", leaseID)
}
