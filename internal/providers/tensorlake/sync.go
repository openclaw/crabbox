package tensorlake

import (
	"context"
	"io"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// rejectIncompatibleSyncOptions refuses Crabbox sync flags whose semantics
// can't be honored on top of `tensorlake sbx cp`. SyncOnly and ChecksumSync
// require Crabbox-side rsync semantics that the Tensorlake CLI doesn't
// expose. ForceSyncLarge is rejected by the core delegated-sync gate before
// we get here, so we don't repeat that check.
func rejectIncompatibleSyncOptions(req RunRequest) error {
	if req.SyncOnly {
		return exit(2, "provider=tensorlake uses archive sync; --sync-only is not supported")
	}
	if req.ChecksumSync {
		return exit(2, "provider=tensorlake uses archive sync; --checksum is not supported")
	}
	return nil
}

func (b *tensorlakeBackend) prepareArchive(ctx context.Context, req RunRequest) (*core.PreparedArchive, error) {
	return core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
		Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
		TempPattern: "crabbox-tensorlake-sync-*.tgz", Stderr: b.rt.Stderr, Now: b.now,
	})
}

func (b *tensorlakeBackend) syncWorkspace(ctx context.Context, cli *tensorlakeCLI, sandboxID string, req RunRequest, workdir string, prepared ...*core.PreparedArchive) ([]timingPhase, time.Duration, error) {
	var archive *core.PreparedArchive
	if len(prepared) > 0 {
		archive = prepared[0]
	}
	if archive == nil {
		var err error
		archive, err = b.prepareArchive(ctx, req)
		if err != nil {
			return nil, 0, err
		}
	}
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
		Workdir: workdir, RemoteArchivePrefix: "crabbox-tensorlake-sync-",
		Provider: providerName, PhaseName: "tensorlake_sync", Stderr: b.rt.Stderr, Now: b.now,
		Upload: func(uploadCtx context.Context, remoteArchive string, _ io.Reader) error {
			return cli.uploadFile(uploadCtx, sandboxID, archive.File.Name(), remoteArchive)
		},
		Exec: func(execCtx context.Context, command string) error { return cli.execShell(execCtx, sandboxID, command) },
	}, archive)
}

func (b *tensorlakeBackend) prepareWorkspace(ctx context.Context, cli *tensorlakeCLI, sandboxID, workdir string) error {
	return cli.execShell(ctx, sandboxID, "mkdir -p "+shellQuote(workdir))
}
