package agentsandbox

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/openclaw/crabbox/internal/providers/shared"
)

func lockAgentSandboxLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	if !strings.HasPrefix(leaseID, leasePrefix) || strings.TrimPrefix(leaseID, leasePrefix) == "" || filepath.Base(leaseID) != leaseID || leaseID == "." {
		return nil, exit(2, "invalid agent-sandbox lease id %q", leaseID)
	}
	return lockAgentSandboxOperationPath(ctx, leaseID+".agent-sandbox-operation.lock")
}

func lockAgentSandboxSlugAllocation(ctx context.Context, _ string) (func(), error) {
	return lockAgentSandboxOperationPath(ctx, "agent-sandbox-slug-allocation.lock")
}

func lockAgentSandboxOperationPath(ctx context.Context, name string) (func(), error) {
	return shared.LockOperation(ctx, "agent-sandbox", name, "agent-sandbox operation "+name)
}
