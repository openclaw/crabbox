package codesandbox

import (
	"context"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *codeSandboxBackend) syncWorkspace(ctx context.Context, api codeSandboxAPI, sandboxID string, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config:              b.cfg,
		Repo:                req.Repo,
		ForceSyncLarge:      req.ForceSyncLarge,
		Workdir:             workdir,
		TempPattern:         "crabbox-codesandbox-sync-*.tgz",
		RemoteArchiveDir:    defaultWorkdir,
		RemoteArchivePrefix: ".crabbox-codesandbox-sync-",
		PhaseName:           "codesandbox_sync",
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

func (b *codeSandboxBackend) execShell(ctx context.Context, api codeSandboxAPI, sandboxID, command string) error {
	res, err := api.RunCommand(ctx, sandboxID, CommandRequest{
		Command: []string{"bash", "-lc", command},
		Timeout: b.execTimeoutSecs(),
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return exit(res.ExitCode, "codesandbox exec %q exited %d: %s", command, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (b *codeSandboxBackend) ensureWorkspace(ctx context.Context, api codeSandboxAPI, sandboxID, workdir string) error {
	return b.execShell(ctx, api, sandboxID, "mkdir -p "+shellQuote(workdir))
}
