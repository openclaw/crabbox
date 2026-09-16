package awslambdamicrovm

import (
	"context"
	"io"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) workspace(runner runnerAPI, vm microVM, req core.RunRequest) core.ArchiveWorkspace {
	workspace := core.NewArchiveWorkspace(b.cfg, b.rt, req, providerName, b.cfg.AWSLambdaMicroVM.Workdir)
	workspace.RemoteArchiveDir = "/tmp"
	workspace.RemoteArchivePrefix = "crabbox-sync-"
	workspace.Upload = func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
		return runner.Upload(uploadCtx, vm, remoteArchive, body)
	}
	workspace.Exec = func(execCtx context.Context, command string) error {
		exitCode, err := runner.Exec(execCtx, vm, command, "/", nil, io.Discard, b.rt.Stderr)
		if err != nil {
			return err
		}
		if exitCode != 0 {
			return core.Exit(exitCode, "%s sync command exited %d", providerName, exitCode)
		}
		return nil
	}
	return workspace
}
