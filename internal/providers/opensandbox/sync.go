package opensandbox

import (
	"context"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *openSandboxBackend) syncWorkspace(ctx context.Context, api openSandboxClient, sandboxID string, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config:              b.cfg,
		Repo:                req.Repo,
		ForceSyncLarge:      req.ForceSyncLarge,
		Workdir:             workdir,
		TempPattern:         "crabbox-opensandbox-sync-*.tgz",
		RemoteArchiveDir:    "/tmp",
		RemoteArchivePrefix: "crabbox-sync-",
		PhaseName:           "opensandbox_sync",
		Provider:            providerName,
		Stderr:              b.rt.Stderr,
		Now:                 b.now,
		CleanupContext:      b.cleanupContext,
		Upload: func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
			return api.UploadFile(uploadCtx, sandboxID, remoteArchive, body)
		},
		Exec: func(execCtx context.Context, command string) error {
			return b.execShell(execCtx, api, sandboxID, command)
		},
	})
}

func (b *openSandboxBackend) execShell(ctx context.Context, api openSandboxClient, sandboxID, command string) error {
	exitCode, err := api.RunCommand(ctx, sandboxID, runCommandRequest{
		Command:     "sh -lc " + shellQuote(command),
		TimeoutSecs: b.execTimeoutSecs(),
	})
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return exit(exitCode, "opensandbox exec %q exited %d", command, exitCode)
	}
	return nil
}

func (b *openSandboxBackend) ensureWorkspace(ctx context.Context, api openSandboxClient, sandboxID, workdir string) error {
	return b.execShell(ctx, api, sandboxID, "mkdir -p "+shellQuote(workdir))
}
