package githubcodespaces

import (
	"context"
	"os"
	"path/filepath"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

var ensureGitHubCodespacesClaimNamespace = func(string) error {
	return core.EnsureCrabboxClaimNamespaceDurable()
}

func lockGitHubCodespacesLeaseOperation(ctx context.Context, leaseID string) (func(), error) {
	if !core.IsCanonicalLeaseID(leaseID) {
		return nil, exit(2, "invalid github-codespaces lease id %q", leaseID)
	}
	return lockGitHubCodespacesOperation(
		ctx,
		leaseID+".github-codespaces-operation.lock",
		"github-codespaces lease "+leaseID,
	)
}

func lockGitHubCodespacesSlugAllocation(ctx context.Context) (func(), error) {
	return lockGitHubCodespacesOperation(
		ctx,
		"github-codespaces-slug-allocation.lock",
		"github-codespaces slug allocation",
	)
}

func lockGitHubCodespacesOperation(ctx context.Context, lockName, description string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		return nil, err
	}
	stateDir = filepath.Clean(stateDir)
	if !filepath.IsAbs(stateDir) {
		return nil, exit(2, "github-codespaces state directory must be absolute: %s", stateDir)
	}
	if err := ensureGitHubCodespacesClaimNamespace(stateDir); err != nil {
		return nil, exit(2, "create github-codespaces claim namespace: %v", err)
	}
	lockDir := filepath.Join(stateDir, "claim-locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, exit(2, "create github-codespaces lock directory: %v", err)
	}
	lockPath := filepath.Join(lockDir, lockName)
	return shared.LockOperationFile(ctx, lockPath, description)
}
