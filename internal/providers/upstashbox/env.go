package upstashbox

import (
	"context"
	"fmt"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

// An envFileReceipt authorizes only this operation's profile, never Box deletion
// or claim publication. Discovery-only runs do not require a local claim.
type envFileReceipt struct {
	client      api
	box         boxData
	leaseID     string
	claim       core.LeaseClaim
	claimExists bool
}

func captureEnvFileReceipt(ctx context.Context, client api, leaseID, boxID, slug string) (envFileReceipt, error) {
	receipt := envFileReceipt{client: client, leaseID: leaseID}
	err := core.WithDurableLeaseClaimLockContext(ctx, leaseID, func(claim *core.LeaseClaim, exists bool, _ func() error) error {
		box, err := client.GetBox(ctx, boxID)
		if err != nil {
			return err
		}
		if box.ID != boxID || !isCrabboxBox(box) || boxLeaseID(box) != leaseID || boxSlug(leaseID, box) != slug || box.CreatedAt <= 0 {
			return fmt.Errorf("upstash-box environment profile requires complete matching Box identity for %s", boxID)
		}
		receipt.box, receipt.claim, receipt.claimExists = box, *claim, exists
		return nil
	})
	return receipt, err
}

func (r envFileReceipt) withUnchanged(ctx context.Context, action func() error) error {
	guarded := func() error {
		box, err := r.client.GetBox(ctx, r.box.ID)
		if err != nil {
			return err
		}
		if box.ID != r.box.ID || box.Name != r.box.Name || box.CreatedAt != r.box.CreatedAt {
			return fmt.Errorf("upstash-box environment profile Box identity changed for %s", r.box.ID)
		}
		return action()
	}
	if r.claimExists {
		return core.WithLeaseClaimUnchangedShared(ctx, r.leaseID, r.claim, guarded)
	}
	return core.WithDurableLeaseClaimLockContext(ctx, r.leaseID, func(_ *core.LeaseClaim, exists bool, _ func() error) error {
		if exists {
			return fmt.Errorf("lease %s claim changed; retry", r.leaseID)
		}
		return guarded()
	})
}

func uploadEnvProfile(ctx context.Context, client api, leaseID, boxID, slug string, env map[string]string) (string, func() error, error) {
	receipt, err := captureEnvFileReceipt(ctx, client, leaseID, boxID, slug)
	if err != nil {
		return "", nil, err
	}
	profile, err := shared.PrepareShellEnvProfile(env, workspaceRoot, ".crabbox-env-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upstashBoxCleanupTimeout)
		defer cancel()
		err := profile.Close(cleanupCtx, func(removeCtx context.Context, remotePath string) error {
			return receipt.withUnchanged(removeCtx, func() error {
				result, err := client.Exec(removeCtx, boxID, "rm -f "+shellQuote(remotePath), "")
				if err != nil {
					return err
				}
				if result.ExitCode != 0 {
					return commandExitError("upstash-box exec rm -f "+remotePath, result)
				}
				return nil
			})
		})
		if err != nil {
			return fmt.Errorf("upstash-box env cleanup failed for %s: %w", boxID, err)
		}
		return nil
	}
	// Admission encloses Upload so a refused write never acquires remote custody.
	err = receipt.withUnchanged(ctx, func() error {
		return profile.Upload(ctx, func(uploadCtx context.Context, localPath, remotePath string) error {
			return client.UploadFile(uploadCtx, boxID, localPath, remotePath)
		})
	})
	return profile.RemotePath(), cleanup, err
}
