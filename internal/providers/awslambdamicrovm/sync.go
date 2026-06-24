package awslambdamicrovm

import (
	"context"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) syncWorkspace(ctx context.Context, runner runnerAPI, vm microVM, req RunRequest) ([]timingPhase, time.Duration, error) {
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config:              b.cfg,
		Repo:                req.Repo,
		ForceSyncLarge:      req.ForceSyncLarge,
		Workdir:             b.cfg.AWSLambdaMicroVM.Workdir,
		TempPattern:         "crabbox-aws-lambda-microvm-sync-*.tgz",
		RemoteArchiveDir:    "/tmp",
		RemoteArchivePrefix: "crabbox-sync-",
		PhaseName:           "aws_lambda_microvm_sync",
		Provider:            providerName,
		Stderr:              b.rt.Stderr,
		Now:                 func() time.Time { return now(b.rt) },
		Upload: func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
			return runner.Upload(uploadCtx, vm, remoteArchive, body)
		},
		Exec: func(execCtx context.Context, command string) error {
			exitCode, err := runner.Exec(execCtx, vm, command, "/", nil, io.Discard, b.rt.Stderr)
			if err != nil {
				return err
			}
			if exitCode != 0 {
				return exit(exitCode, "%s sync command exited %d", providerName, exitCode)
			}
			return nil
		},
	})
}
