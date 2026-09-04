package upstashbox

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) prepareArchive(ctx context.Context, req RunRequest) (*core.PreparedArchive, error) {
	return core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
		Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
		TempPattern: "crabbox-upstash-box-sync-*.tgz", Stderr: b.rt.Stderr, Now: b.now,
	})
}

func (b *backend) syncWorkspace(ctx context.Context, client api, boxID string, req RunRequest, workdir, folder string, prepared ...*core.PreparedArchive) ([]timingPhase, time.Duration, error) {
	expectedFolder, err := workspaceFolder(workdir)
	if err != nil {
		return nil, 0, err
	}
	if folder != expectedFolder || strings.Contains(folder, "..") {
		return nil, 0, exit(2, "upstash-box sync folder does not match workdir")
	}
	var archive *core.PreparedArchive
	if len(prepared) > 0 {
		archive = prepared[0]
	}
	if archive == nil {
		archive, err = b.prepareArchive(ctx, req)
		if err != nil {
			return nil, 0, err
		}
	}
	// Box file APIs use absolute workspace paths; exec runs relative to its
	// workspace root. Keep that translation here, not in the shared lifecycle.
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
		Workdir: folder, RemoteArchiveDir: ".", RemoteArchivePrefix: ".crabbox-upstash-box-sync-",
		Provider: providerName, PhaseName: "upstash_box_sync", Stderr: b.rt.Stderr, Now: b.now,
		CleanupContext: func(context.Context) (context.Context, context.CancelFunc) { return upstashBoxCleanupContext() },
		Upload: func(uploadCtx context.Context, remoteArchive string, _ io.Reader) error {
			if err := client.UploadFile(uploadCtx, boxID, archive.File.Name(), workspacePath(remoteArchive)); err != nil {
				return fmt.Errorf("upstash-box upload archive: %w", err)
			}
			return nil
		},
		Exec: func(execCtx context.Context, command string) error {
			return b.execShell(execCtx, client, boxID, command, io.Discard)
		},
	}, archive)
}

func (b *backend) prepareWorkspace(ctx context.Context, client api, boxID, folder string) error {
	folder = strings.Trim(strings.TrimSpace(folder), "/")
	if folder == "" || strings.Contains(folder, "..") {
		return exit(2, "upstash-box workspace folder %q is invalid", folder)
	}
	return b.execShell(ctx, client, boxID, "mkdir -p "+shellQuote(folder), io.Discard)
}

func (b *backend) execShell(ctx context.Context, client api, boxID, command string, stdout io.Writer) error {
	result, err := client.Exec(ctx, boxID, command, "")
	if err != nil {
		return fmt.Errorf("upstash-box exec %q: %w", command, err)
	}
	if stdout != nil && result.Output != "" {
		_, _ = io.WriteString(stdout, result.Output)
	}
	if result.ExitCode != 0 {
		return commandExitError("upstash-box exec "+command, result)
	}
	return nil
}
