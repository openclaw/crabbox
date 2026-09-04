package smolvm

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

const envProfileCleanupTimeout = 30 * time.Second

func (b *backend) uploadEnvProfile(ctx context.Context, client api, claim core.LeaseClaim, env map[string]string, workdir string) (string, func(context.Context), error) {
	profile, err := shared.PrepareShellEnvProfile(env, workdir, ".crabbox-env-")
	if err != nil {
		return "", nil, err
	}
	authorized := func(operationCtx context.Context, action func() error) error {
		if _, err := b.claimBinding(claim); err != nil {
			return err
		}
		return core.WithLeaseClaimUnchangedShared(operationCtx, claim.LeaseID, claim, func() error {
			machine, err := client.GetMachine(operationCtx, claim.CloudID)
			if err != nil {
				return err
			}
			if err := validateMachineIdentity(machine, machineFromClaim(claim)); err != nil {
				return err
			}
			return action()
		})
	}
	cleanup := func(cleanupCtx context.Context) {
		err := profile.Close(cleanupCtx, func(removeCtx context.Context, remotePath string) error {
			return authorized(removeCtx, func() error {
				return b.execShell(removeCtx, client, claim.CloudID, "rm -f "+shellQuote(remotePath), io.Discard)
			})
		})
		if err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: smolvm env profile cleanup failed for %s: %v\n", claim.CloudID, err)
		}
	}
	err = authorized(ctx, func() error {
		return profile.Upload(ctx, func(uploadCtx context.Context, localPath, remotePath string) error {
			data, err := os.ReadFile(localPath)
			if err != nil {
				return err
			}
			return client.WriteFile(uploadCtx, claim.CloudID, remotePath, string(data))
		})
	})
	if err != nil {
		return "", cleanup, err
	}
	return profile.RemotePath(), cleanup, nil
}
