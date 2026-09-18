package opencomputer

import (
	"context"
	"io"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *openComputerBackend) workspace(api *ocAPIClient, sandboxID string, req core.RunRequest, workdir string) core.ArchiveWorkspace {
	workspace := core.NewArchiveWorkspace(b.cfg, b.rt, req, providerName, workdir)
	workspace.RemoteArchiveDir = "/tmp"
	workspace.RemoteArchivePrefix = "crabbox-sync-"
	workspace.CleanupContext = b.cleanupContext
	workspace.Upload = func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
		return api.uploadFile(uploadCtx, sandboxID, remoteArchive, body)
	}
	workspace.Exec = func(execCtx context.Context, command string) error {
		return b.execShell(execCtx, api, sandboxID, command)
	}
	return workspace
}

func (b *openComputerBackend) execShell(ctx context.Context, api *ocAPIClient, sandboxID, command string) error {
	res, err := api.execRun(ctx, sandboxID, execRunRequest{
		Cmd:     "bash",
		Args:    []string{"-lc", command},
		Timeout: b.execTimeoutSecs(),
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return core.Exit(res.ExitCode, "opencomputer exec %q exited %d: %s", command, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
