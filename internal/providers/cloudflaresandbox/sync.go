package cloudflaresandbox

import (
	"context"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) syncWorkspace(ctx context.Context, api bridgeClient, sandboxID string, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config:              b.cfg,
		Repo:                req.Repo,
		ForceSyncLarge:      req.ForceSyncLarge,
		Workdir:             workdir,
		TempPattern:         "crabbox-cloudflare-sandbox-sync-*.tgz",
		RemoteArchiveDir:    "/tmp",
		RemoteArchivePrefix: "crabbox-cloudflare-sandbox-sync-",
		PhaseName:           "cloudflare_sandbox_sync",
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

func (b *backend) execShell(ctx context.Context, api bridgeClient, sandboxID, command string) error {
	res, err := api.Exec(ctx, sandboxID, execRequest{
		Command:     "sh -lc " + shellQuote(command),
		TimeoutSecs: b.execTimeoutSecs(),
	}, io.Discard, io.Discard)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return exit(res.ExitCode, "cloudflare-sandbox exec %q exited %d: %s", command, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (b *backend) ensureWorkspace(ctx context.Context, api bridgeClient, sandboxID, workdir string) error {
	return b.execShell(ctx, api, sandboxID, "mkdir -p "+shellQuote(workdir))
}
