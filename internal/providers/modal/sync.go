package modal

import (
	"context"
	"fmt"
	"io"
	"os"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *modalBackend) workspace(client modalAPI, sandboxID string, req core.RunRequest, workdir string) core.ArchiveWorkspace {
	workspace := core.NewArchiveWorkspace(b.cfg, b.rt, req, providerName, workdir)
	workspace.RemoteArchivePrefix = "crabbox-modal-sync-"
	workspace.CleanWorkdir = cleanModalWorkdir
	workspace.Upload = func(ctx context.Context, remoteArchive string, archive io.Reader) error {
		// The SDK requires the local file owned by the shared archive transport.
		file, ok := archive.(*os.File)
		if !ok {
			return fmt.Errorf("modal archive upload requires a local file")
		}
		return modalError("upload archive", client.UploadFile(ctx, sandboxID, file.Name(), remoteArchive))
	}
	workspace.Exec = func(ctx context.Context, command string) error {
		return b.execShell(ctx, client, sandboxID, command, io.Discard)
	}
	return workspace
}

func (b *modalBackend) execShell(ctx context.Context, client modalAPI, sandboxID, command string, stdout io.Writer) error {
	code, err := client.Exec(ctx, modalExecRequest{
		SandboxID: sandboxID,
		Command:   []string{"bash", "-lc", command},
		Timeout:   durationSecondsCeil(modalTimeoutDuration(b.cfg.TTL)),
		Stdout:    stdout,
		Stderr:    b.rt.Stderr,
	})
	if err != nil {
		return fmt.Errorf("modal exec %q: %w", command, err)
	}
	if code != 0 {
		return core.Exit(code, "modal exec %q exited %d", command, code)
	}
	return nil
}
