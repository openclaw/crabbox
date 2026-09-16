package cloudflaresandbox

import (
	"context"
	"io"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) workspace(api bridgeClient, sandboxID string, req core.RunRequest, workdir string) core.ArchiveWorkspace {
	workspace := core.NewArchiveWorkspace(b.cfg, b.rt, req, providerName, workdir)
	workspace.RemoteArchivePrefix = "crabbox-cloudflare-sandbox-sync-"
	workspace.CleanupContext = b.cleanupContext
	workspace.Upload = func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
		return api.UploadFile(uploadCtx, sandboxID, remoteArchive, body)
	}
	workspace.Exec = func(execCtx context.Context, command string) error {
		return b.execShell(execCtx, api, sandboxID, command)
	}
	return workspace
}

func (b *backend) execShell(ctx context.Context, api bridgeClient, sandboxID, command string) error {
	res, err := api.Exec(ctx, sandboxID, execRequest{
		Command:     "sh -lc " + core.ShellQuote(command),
		TimeoutSecs: b.execTimeoutSecs(),
	}, io.Discard, io.Discard)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return core.Exit(res.ExitCode, "cloudflare-sandbox exec %q exited %d: %s", command, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
