package crownest

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/openclaw/crabbox/internal/providers/shared"
)

func lockCrownestLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	if !strings.HasPrefix(leaseID, leasePrefix) || strings.TrimPrefix(leaseID, leasePrefix) == "" ||
		filepath.Base(leaseID) != leaseID || leaseID == "." {
		return nil, exit(2, "invalid crownest lease id %q", leaseID)
	}
	return shared.LockLeaseOperation(ctx, "crownest", leaseID)
}
