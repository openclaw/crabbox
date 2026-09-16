package opensandbox

import (
	"context"
	"io"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *openSandboxBackend) workspace(api openSandboxClient, sandboxID string, req core.RunRequest, workdir string) core.ArchiveWorkspace {
	workspace := core.NewArchiveWorkspace(b.cfg, b.rt, req, providerName, workdir)
	workspace.RemoteArchivePrefix = "crabbox-sync-"
	workspace.CleanupContext = b.cleanupContext
	workspace.Upload = func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
		return api.UploadFile(uploadCtx, sandboxID, remoteArchive, body)
	}
	workspace.Exec = func(execCtx context.Context, command string) error {
		return b.execShell(execCtx, api, sandboxID, command)
	}
	return workspace
}

func (b *openSandboxBackend) execShell(ctx context.Context, api openSandboxClient, sandboxID, command string) error {
	exitCode, err := api.RunCommand(ctx, sandboxID, runCommandRequest{
		Command:     "sh -lc " + core.ShellQuote(command),
		TimeoutSecs: b.execTimeoutSecs(),
	})
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return core.Exit(exitCode, "opensandbox exec %q exited %d", command, exitCode)
	}
	return nil
}
