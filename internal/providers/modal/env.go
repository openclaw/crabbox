package modal

import (
	"context"
	"fmt"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *modalBackend) uploadEnvProfile(ctx context.Context, client modalAPI, claim core.LeaseClaim, env map[string]string) (string, func(context.Context), error) {
	profile, err := shared.PrepareShellEnvProfile(env, "crabbox-modal-env-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func(cleanupCtx context.Context) {
		err := profile.Close(cleanupCtx, func(removeCtx context.Context, remotePath string) error {
			// Each operation owns a unique path; shared fencing excludes claim writers
			// without allowing lock contention to outlive the cleanup deadline.
			return core.WithLeaseClaimUnchangedShared(removeCtx, claim.LeaseID, claim, func() error {
				return b.execShell(removeCtx, client, claim.CloudID, "rm -f "+shellQuote(remotePath), nil)
			})
		})
		if err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: modal env profile cleanup failed for %s: %v\n", claim.CloudID, err)
		}
	}
	if err := profile.Upload(ctx, func(uploadCtx context.Context, localPath, remotePath string) error {
		return client.UploadFile(uploadCtx, claim.CloudID, localPath, remotePath)
	}); err != nil {
		return "", cleanup, err
	}
	return profile.RemotePath(), cleanup, nil
}
