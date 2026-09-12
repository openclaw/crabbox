package cloudflaresandbox

import (
	"context"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func lockCloudflareSandboxLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	if !strings.HasPrefix(leaseID, leasePrefix) || strings.TrimPrefix(leaseID, leasePrefix) == "" || filepath.Base(leaseID) != leaseID || leaseID == "." {
		return nil, core.Exit(2, "invalid cloudflare-sandbox lease id %q", leaseID)
	}
	return shared.LockLeaseOperation(ctx, "cloudflare-sandbox", leaseID)
}
