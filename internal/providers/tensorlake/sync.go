package tensorlake

import (
	"context"
	"io"
	"os"
	"path"
	"strings"
	"time"
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

// syncWorkspace builds a gzipped tar archive of the repo, uploads it via
// `tensorlake sbx cp`, and extracts it into the configured workdir. Returns
// per-phase timings so warmup/run summaries can break the sync down the same
// way islo and daytona do.
func (b *tensorlakeBackend) syncWorkspace(ctx context.Context, cli *tensorlakeCLI, sandboxID string, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
	start := b.now()

	excludes, err := syncExcludes(req.Repo.Root, b.cfg)
	if err != nil {
		return nil, 0, err
	}

	manifestStart := b.now()
	manifest, err := syncManifest(req.Repo.Root, excludes, b.cfg.Sync.Includes)
	if err != nil {
		return nil, 0, exit(6, "build sync file list: %v", err)
	}
	manifestDuration := b.now().Sub(manifestStart)

	preflightStart := b.now()
	if err := checkSyncPreflight(manifest, b.cfg, req.ForceSyncLarge, b.rt.Stderr); err != nil {
		return nil, 0, err
	}
	preflightDuration := b.now().Sub(preflightStart)

	prepareStart := b.now()
	if err := b.prepareWorkspace(ctx, cli, sandboxID, workdir); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStart)

	archiveStart := b.now()
	archive, err := createTensorlakeSyncArchive(ctx, req.Repo, manifest, b.rt.Stderr)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	archiveDuration := b.now().Sub(archiveStart)

	uploadStart := b.now()
	remoteArchive := path.Join("/tmp", "crabbox-sync-"+randomSuffix()+".tgz")
	if err := cli.uploadFile(ctx, sandboxID, archive.Name(), remoteArchive); err != nil {
		return nil, 0, err
	}
	uploadDuration := b.now().Sub(uploadStart)

	extractStart := b.now()
	if err := b.extractRemoteArchive(ctx, cli, sandboxID, remoteArchive, workdir); err != nil {
		// Best-effort cleanup of the remote archive even when extract fails.
		_ = cli.execShell(context.Background(), sandboxID, "rm -f "+shellQuote(remoteArchive))
		return nil, 0, err
	}
	extractDuration := b.now().Sub(extractStart)

	total := b.now().Sub(start)
	return []timingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "prepare", Ms: prepareDuration.Milliseconds()},
		{Name: "archive", Ms: archiveDuration.Milliseconds()},
		{Name: "upload", Ms: uploadDuration.Milliseconds()},
		{Name: "extract", Ms: extractDuration.Milliseconds()},
		{Name: "tensorlake_sync", Ms: total.Milliseconds()},
	}, total, nil
}

func (b *tensorlakeBackend) prepareWorkspace(ctx context.Context, cli *tensorlakeCLI, sandboxID, workdir string) error {
	return cli.execShell(ctx, sandboxID, "mkdir -p "+shellQuote(workdir))
}

func (b *tensorlakeBackend) extractRemoteArchive(ctx context.Context, cli *tensorlakeCLI, sandboxID, remoteArchive, workdir string) error {
	return cli.execShell(ctx, sandboxID, tensorlakeExtractArchiveCommand(workdir, remoteArchive, b.cfg.Sync.Delete))
}

func tensorlakeExtractArchiveCommand(workdir, remoteArchive string, deleteExisting bool) string {
	if !deleteExisting {
		return strings.Join([]string{
			"mkdir -p " + shellQuote(workdir) + " && tar -xzf " + shellQuote(remoteArchive) + " -C " + shellQuote(workdir),
			"crabbox_status=$?",
			"rm -f " + shellQuote(remoteArchive),
			"exit \"$crabbox_status\"",
		}, "; ")
	}
	parent := path.Dir(workdir)
	tmp := path.Join(parent, ".crabbox-sync-"+randomSuffix())
	backup := path.Join(parent, ".crabbox-backup-"+randomSuffix())
	steps := []string{
		"(",
		"rm -rf " + shellQuote(tmp) + " " + shellQuote(backup) + " &&",
		"mkdir -p " + shellQuote(tmp) + " &&",
		"tar -xzf " + shellQuote(remoteArchive) + " -C " + shellQuote(tmp) + " &&",
		"if [ -e " + shellQuote(workdir) + " ]; then mv " + shellQuote(workdir) + " " + shellQuote(backup) + "; fi &&",
		"if mv " + shellQuote(tmp) + " " + shellQuote(workdir) + "; then",
		"rm -rf " + shellQuote(backup) + ";",
		"else",
		"crabbox_swap_status=$?;",
		"if [ -e " + shellQuote(backup) + " ]; then mv " + shellQuote(backup) + " " + shellQuote(workdir) + " || true; fi;",
		"exit \"$crabbox_swap_status\";",
		"fi",
		");",
		"crabbox_status=$?;",
		"rm -rf " + shellQuote(tmp) + ";",
		"rm -f " + shellQuote(remoteArchive) + ";",
		"exit \"$crabbox_status\"",
	}
	return strings.Join(steps, " ")
}

func createTensorlakeSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, _ io.Writer) (*os.File, error) {
	return createPortableSyncArchive(ctx, repo, manifest, "crabbox-tensorlake-sync-*.tgz")
}
