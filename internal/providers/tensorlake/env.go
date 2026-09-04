package tensorlake

import (
	"context"
	"fmt"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"time"
)

const envProfileCleanupTimeout = 30 * time.Second

func (b *tensorlakeBackend) uploadEnvProfile(ctx context.Context, cli *tensorlakeCLI, claim core.LeaseClaim, env map[string]string) (string, func(context.Context), error) {
	profile, err := shared.PrepareShellEnvProfile(env, "crabbox-env-")
	if err != nil {
		return "", nil, err
	}
	authorized := func(operationCtx context.Context, action func() error) error {
		_, binding, err := bindingForClaim(claim)
		if err != nil {
			return err
		}
		return core.WithLeaseClaimUnchangedShared(operationCtx, claim.LeaseID, claim, func() error {
			item, err := cli.verifyBinding(operationCtx, binding)
			if err != nil {
				return err
			}
			if item.State == "terminated" {
				return exit(2, "Tensorlake sandbox has terminated")
			}
			return action()
		})
	}
	cleanup := func(cleanupCtx context.Context) {
		err := profile.Close(cleanupCtx, func(removeCtx context.Context, remotePath string) error {
			return authorized(removeCtx, func() error { return cli.execShell(removeCtx, claim.CloudID, "rm -f "+shellQuote(remotePath)) })
		})
		if err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: tensorlake env profile cleanup failed for %s: %v\n", claim.CloudID, err)
		}
	}
	err = authorized(ctx, func() error {
		return profile.Upload(ctx, func(uploadCtx context.Context, localPath, remotePath string) error {
			return cli.uploadFile(uploadCtx, claim.CloudID, localPath, remotePath)
		})
	})
	if err != nil {
		return "", cleanup, err
	}
	return profile.RemotePath(), cleanup, nil
}
