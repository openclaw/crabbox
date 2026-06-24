package nomad

import (
	"context"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) syncWorkspace(ctx context.Context, client Client, ready allocationReadiness, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
	syncReq := core.DelegatedArchiveSyncRequest{
		Config:              b.cfg,
		Repo:                req.Repo,
		ForceSyncLarge:      req.ForceSyncLarge,
		Workdir:             workdir,
		TempPattern:         "crabbox-nomad-sync-*.tgz",
		RemoteArchiveDir:    "/tmp",
		RemoteArchivePrefix: ".crabbox-nomad-sync-",
		PhaseName:           "nomad_sync",
		Provider:            providerName,
		Stderr:              b.rt.Stderr,
		Now:                 b.now,
		CleanupContext:      b.cleanupContext,
		Upload: func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
			return b.uploadArchive(uploadCtx, client, ready, remoteArchive, body)
		},
		Exec: func(execCtx context.Context, command string) error {
			return b.execShell(execCtx, client, ready, command)
		},
	}
	return core.RunDelegatedArchiveSync(ctx, syncReq)
}

func (b *backend) uploadArchive(ctx context.Context, client Client, ready allocationReadiness, remoteArchive string, body io.Reader) error {
	command := "mkdir -p " + shellQuote("/tmp") + " && cat > " + shellQuote(remoteArchive)
	execCtx, cancel := b.execContext(ctx)
	defer cancel()
	exitCode, err := b.allocationExec(execCtx, client, ready, []string{"sh", "-lc", command}, body, b.rt.Stdout, b.rt.Stderr)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return exit(exitCode, "nomad archive upload exited %d", exitCode)
	}
	return nil
}
