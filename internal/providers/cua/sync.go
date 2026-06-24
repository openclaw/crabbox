package cua

import (
	"context"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b backend) syncWorkspace(ctx context.Context, client *bridgeClient, sandboxID string, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config:              b.cfg,
		Repo:                req.Repo,
		ForceSyncLarge:      req.ForceSyncLarge,
		Workdir:             workdir,
		TempPattern:         "crabbox-cua-sync-*.tgz",
		RemoteArchiveDir:    "/tmp",
		RemoteArchivePrefix: "crabbox-cua-sync-",
		PhaseName:           "cua_sync",
		Provider:            providerName,
		Stderr:              b.rt.Stderr,
		Now:                 b.now,
		CleanupContext:      b.cleanupContext,
		Upload: func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
			file, err := bridgeFileFromReader(remoteArchive, body)
			if err != nil {
				return err
			}
			return client.UploadBytes(uploadCtx, sandboxID, file)
		},
		Exec: func(execCtx context.Context, command string) error {
			return b.execShell(execCtx, client, sandboxID, command)
		},
	})
}

func (b backend) execShell(ctx context.Context, client *bridgeClient, sandboxID, command string) error {
	resp, err := client.Exec(ctx, sandboxID, []string{"bash", "-lc", command}, "", nil, nil, nil)
	if err != nil {
		return err
	}
	if resp.ExitCode != 0 {
		return exit(resp.ExitCode, "CUA exec %q exited %d: %s", command, resp.ExitCode, strings.TrimSpace(resp.Stderr))
	}
	return nil
}

func (b backend) ensureWorkspace(ctx context.Context, client *bridgeClient, sandboxID, workdir string) error {
	return b.execShell(ctx, client, sandboxID, "mkdir -p "+shellQuote(workdir))
}
