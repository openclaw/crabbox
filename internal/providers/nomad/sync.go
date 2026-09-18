package nomad

import (
	"context"
	"io"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) workspace(client Client, ready allocationReadiness, req RunRequest, workdir string) core.ArchiveWorkspace {
	workspace := core.NewArchiveWorkspace(b.cfg, b.rt, req, providerName, workdir)
	workspace.RemoteArchiveDir = "/tmp"
	workspace.RemoteArchivePrefix = ".crabbox-nomad-sync-"
	workspace.CleanupContext = b.cleanupContext
	workspace.Upload = func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
		return b.uploadArchive(uploadCtx, client, ready, remoteArchive, body)
	}
	workspace.Exec = func(execCtx context.Context, command string) error {
		return b.execShell(execCtx, client, ready, command)
	}
	return workspace
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
