package awslambdamicrovm

import (
	"context"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const leasePrefix = "cbx_"

func lockAWSLambdaMicroVMLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	if !strings.HasPrefix(leaseID, leasePrefix) || strings.TrimPrefix(leaseID, leasePrefix) == "" ||
		filepath.Base(leaseID) != leaseID || leaseID == "." {
		return nil, core.Exit(2, "invalid aws-lambda-microvm lease id %q", leaseID)
	}
	return shared.LockLeaseOperation(ctx, "aws-lambda-microvm", leaseID)
}
