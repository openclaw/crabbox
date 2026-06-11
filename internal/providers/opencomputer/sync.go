package opencomputer

import (
	"context"
	"os"
	"path"
	"strings"
	"time"
)

// syncWorkspace builds a gzipped tar archive of the repo, uploads it into the
// sandbox via the file API (`PUT /files`, content in the request body — never
// argv), and extracts it into the configured workdir via `exec/run`. Returns
// per-phase timings so warmup/run summaries break the sync down the same way
// islo and tensorlake do.
func (b *openComputerBackend) syncWorkspace(ctx context.Context, api *ocAPIClient, sandboxID string, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
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
	if err := b.prepareWorkspace(ctx, api, sandboxID, workdir); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStart)

	archiveStart := b.now()
	archive, err := createOpenComputerSyncArchive(ctx, req.Repo, manifest)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	archiveDuration := b.now().Sub(archiveStart)

	uploadStart := b.now()
	if _, err := archive.Seek(0, 0); err != nil {
		return nil, 0, exit(6, "rewind sync archive: %v", err)
	}
	remoteArchive := path.Join("/tmp", "crabbox-sync-"+randomSuffix()+".tgz")
	if err := api.uploadFile(ctx, sandboxID, remoteArchive, archive); err != nil {
		return nil, 0, err
	}
	uploadDuration := b.now().Sub(uploadStart)

	extractStart := b.now()
	// Extract must succeed; cleanup is best-effort. The uploaded archive is
	// written by the file API as a different owner than the exec user, so a
	// `rm` may be denied — and it does not matter on an ephemeral box.
	if err := b.execShell(ctx, api, sandboxID, "tar -xzf "+shellQuote(remoteArchive)+" -C "+shellQuote(workdir)); err != nil {
		return nil, 0, err
	}
	cleanupCtx, cancel := b.cleanupContext(ctx)
	defer cancel()
	_ = b.execShell(cleanupCtx, api, sandboxID, "rm -f "+shellQuote(remoteArchive)+" 2>/dev/null || true")
	extractDuration := b.now().Sub(extractStart)

	total := b.now().Sub(start)
	return []timingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "prepare", Ms: prepareDuration.Milliseconds()},
		{Name: "archive", Ms: archiveDuration.Milliseconds()},
		{Name: "upload", Ms: uploadDuration.Milliseconds()},
		{Name: "extract", Ms: extractDuration.Milliseconds()},
		{Name: "opencomputer_sync", Ms: total.Milliseconds()},
	}, total, nil
}

func (b *openComputerBackend) prepareWorkspace(ctx context.Context, api *ocAPIClient, sandboxID, workdir string) error {
	if !b.cfg.Sync.Delete {
		return b.ensureWorkspace(ctx, api, sandboxID, workdir)
	}
	command := "rm -rf " + shellQuote(workdir) + " && mkdir -p " + shellQuote(workdir)
	return b.execShell(ctx, api, sandboxID, command)
}

func (b *openComputerBackend) ensureWorkspace(ctx context.Context, api *ocAPIClient, sandboxID, workdir string) error {
	// --no-sync must never apply sync.delete to a retained workspace.
	return b.execShell(ctx, api, sandboxID, "mkdir -p "+shellQuote(workdir))
}

// execShell runs `bash -lc <command>` via exec/run and returns an error for
// non-zero exits. Used for sync helpers (mkdir, extract, cleanup).
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
		return exit(res.ExitCode, "opencomputer exec %q exited %d: %s", command, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func createOpenComputerSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest) (*os.File, error) {
	return createPortableSyncArchive(ctx, repo, manifest, "crabbox-opencomputer-sync-*.tgz")
}
