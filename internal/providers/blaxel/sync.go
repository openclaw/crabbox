package blaxel

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) syncWorkspace(ctx context.Context, client Client, sandboxID string, req RunRequest, workdir string, prepared *core.PreparedArchive) ([]timingPhase, time.Duration, error) {
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
		Workdir: workdir, TempPattern: "crabbox-blaxel-sync-*.tgz",
		RemoteArchiveDir: "/tmp", RemoteArchivePrefix: "crabbox-blaxel-sync-",
		PhaseName: "blaxel_sync", Provider: providerName, Stderr: b.rt.Stderr,
		Now: func() time.Time { return now(b.rt) }, CleanupContext: b.cleanupContext,
		Upload: func(ctx context.Context, remotePath string, body io.Reader) error {
			if err := client.UploadFile(ctx, sandboxID, remotePath, body); err != nil {
				return fmt.Errorf("blaxel upload archive: %w", err)
			}
			return nil
		},
		Exec: func(ctx context.Context, command string) error {
			return b.execShell(ctx, client, sandboxID, command)
		},
	}, prepared)
}

func (b *backend) ensureWorkspace(ctx context.Context, client Client, sandboxID, workdir string) error {
	return b.execShell(ctx, client, sandboxID, "mkdir -p "+shellQuote(workdir))
}

func (b *backend) execShell(ctx context.Context, client Client, sandboxID, command string) error {
	res, err := client.ExecuteProcess(ctx, sandboxID, ExecuteProcessRequest{
		Command:     "bash",
		Args:        []string{"-lc", command},
		TimeoutSecs: b.execTimeoutSecs(),
	})
	if err != nil {
		return err
	}
	res, err = b.waitProcess(ctx, client, sandboxID, res)
	if err != nil {
		return err
	}
	if res.ExitCode != nil && *res.ExitCode == 0 {
		return nil
	}
	logs, logErr := client.GetProcessLogs(ctx, sandboxID, res.ID)
	if logErr != nil {
		return logErr
	}
	code := 1
	if res.ExitCode != nil {
		code = *res.ExitCode
	}
	return exit(code, "blaxel exec %q exited %d: %s", command, code, strings.TrimSpace(logs.Stderr))
}
