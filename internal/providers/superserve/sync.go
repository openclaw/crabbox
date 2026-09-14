package superserve

import (
	"context"
	"io"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) workspace(api superserveClient, access *sandboxAccess, req core.RunRequest, workdir string) core.ArchiveWorkspace {
	workspace := core.NewArchiveWorkspace(b.cfg, b.rt, req, providerName, workdir)
	workspace.RemoteArchiveDir = "/tmp"
	workspace.RemoteArchivePrefix = "crabbox-sync-"
	workspace.CleanupContext = b.cleanupContext
	workspace.Upload = func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
		return api.UploadFile(uploadCtx, access, remoteArchive, body)
	}
	workspace.Exec = func(execCtx context.Context, command string) error {
		return b.execShell(execCtx, api, access, command)
	}
	return workspace
}

func (b *backend) execShell(ctx context.Context, api superserveClient, access *sandboxAccess, command string) error {
	res, err := api.Exec(ctx, access, execRequest{
		Command:     "sh -lc " + core.ShellQuote(command),
		TimeoutSecs: b.execTimeoutSecs(),
	}, io.Discard, io.Discard)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return core.Exit(res.ExitCode, "superserve exec %q exited %d: %s", command, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
