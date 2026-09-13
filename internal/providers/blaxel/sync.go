package blaxel

import (
	"context"
	"fmt"
	"io"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) workspace(client Client, sandboxID string, req core.RunRequest, workdir string) core.ArchiveWorkspace {
	workspace := core.NewArchiveWorkspace(b.cfg, b.rt, req, providerName, workdir)
	workspace.RemoteArchiveDir = "/tmp"
	workspace.RemoteArchivePrefix = "crabbox-blaxel-sync-"
	workspace.CleanupContext = b.cleanupContext
	workspace.Upload = func(ctx context.Context, remotePath string, body io.Reader) error {
		if err := client.UploadFile(ctx, sandboxID, remotePath, body); err != nil {
			return fmt.Errorf("blaxel upload archive: %w", err)
		}
		return nil
	}
	workspace.Exec = func(ctx context.Context, command string) error {
		return b.execShell(ctx, client, sandboxID, command)
	}
	return workspace
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
	return core.Exit(code, "blaxel exec %q exited %d: %s", command, code, strings.TrimSpace(logs.Stderr))
}
