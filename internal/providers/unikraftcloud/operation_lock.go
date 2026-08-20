package unikraftcloud

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/openclaw/crabbox/internal/providers/shared"
)

func lockUnikraftCloudLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	if !strings.HasPrefix(leaseID, leasePrefix) || strings.TrimPrefix(leaseID, leasePrefix) == "" ||
		strings.ContainsAny(leaseID, `/\`) || filepath.Base(leaseID) != leaseID || leaseID == "." {
		return nil, exit(2, "invalid unikraft-cloud lease id %q", leaseID)
	}
	return shared.LockLeaseOperation(ctx, "unikraft-cloud", leaseID)
}

func lockUnikraftCloudSlugAllocation(ctx context.Context) (func(), error) {
	return lockUnikraftCloudOperation(ctx, "unikraft-cloud-slug-allocation.lock", "unikraft-cloud slug allocation")
}

func lockUnikraftCloudOperation(ctx context.Context, lockName, description string) (func(), error) {
	return shared.LockOperation(ctx, "unikraft-cloud", lockName, description)
}
