package modal

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *modalBackend) syncWorkspace(ctx context.Context, client modalAPI, sandboxID string, req RunRequest, workdir string, prepared ...*core.PreparedArchive) ([]timingPhase, time.Duration, error) {
	workdir, err := cleanModalWorkdir(workdir)
	if err != nil {
		return nil, 0, err
	}
	return shared.RunSandboxArchiveSync(ctx, shared.SandboxArchiveSyncRequest{
		Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge, Workdir: workdir,
		TempPattern: "crabbox-modal-sync-*.tgz", RemoteArchivePrefix: "crabbox-modal-sync-",
		PhaseName: "modal_sync", Provider: providerName, Stderr: b.rt.Stderr, Now: b.now,
		Upload: func(ctx context.Context, remoteArchive string, archive io.Reader) error {
			// The Modal SDK upload requires a local path. The shared archive
			// transport owns this file and keeps it open until upload completes.
			file, ok := archive.(*os.File)
			if !ok {
				return fmt.Errorf("modal archive upload requires a local file")
			}
			return modalError("upload archive", client.UploadFile(ctx, sandboxID, file.Name(), remoteArchive))
		},
		Exec: func(ctx context.Context, command string) error {
			return b.execShell(ctx, client, sandboxID, command, io.Discard)
		},
	}, prepared...)
}

func (b *modalBackend) prepareWorkspace(ctx context.Context, client modalAPI, sandboxID, workdir string) error {
	workdir, err := cleanModalWorkdir(workdir)
	if err != nil {
		return err
	}
	return b.execShell(ctx, client, sandboxID, "mkdir -p "+shellQuote(workdir), io.Discard)
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
		return exit(code, "modal exec %q exited %d", command, code)
	}
	return nil
}
