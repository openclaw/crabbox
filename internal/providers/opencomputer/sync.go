package opencomputer

import (
	"context"
	"fmt"
	"io"
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
	syncCtx := ctx
	cancel := func() {}
	if b.cfg.Sync.Timeout > 0 {
		syncCtx, cancel = context.WithTimeout(ctx, b.cfg.Sync.Timeout)
	}
	defer cancel()

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
	if err := checkOpenComputerSyncPreflight(manifest, b.cfg, req.ForceSyncLarge, b.rt.Stderr); err != nil {
		return nil, 0, err
	}
	preflightDuration := b.now().Sub(preflightStart)

	archiveStart := b.now()
	archive, err := createOpenComputerSyncArchive(syncCtx, req.Repo, manifest)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	archiveDuration := b.now().Sub(archiveStart)

	remoteArchive := path.Join("/tmp", "crabbox-sync-"+randomSuffix()+".tgz")
	extractDir := workdir
	stagingDir := ""
	if b.cfg.Sync.Delete {
		stagingDir = path.Join(path.Dir(workdir), "."+path.Base(workdir)+".crabbox-sync-"+randomSuffix())
		extractDir = stagingDir
	}
	cleanupRemote := func() {
		cleanupCtx, cancel := b.cleanupContext(ctx)
		defer cancel()
		command := "rm -f " + shellQuote(remoteArchive) + " 2>/dev/null || true"
		if stagingDir != "" {
			command += "; rm -rf " + shellQuote(stagingDir) + " 2>/dev/null || true"
		}
		_ = b.execShell(cleanupCtx, api, sandboxID, command)
	}
	cleanupPending := true
	defer func() {
		if cleanupPending {
			cleanupRemote()
		}
	}()

	uploadStart := b.now()
	if _, err := archive.Seek(0, 0); err != nil {
		return nil, 0, exit(6, "rewind sync archive: %v", err)
	}
	if err := api.uploadFile(syncCtx, sandboxID, remoteArchive, archive); err != nil {
		return nil, 0, err
	}
	uploadDuration := b.now().Sub(uploadStart)

	prepareStart := b.now()
	if stagingDir == "" {
		err = b.ensureWorkspace(syncCtx, api, sandboxID, workdir)
	} else {
		err = b.execShell(syncCtx, api, sandboxID, "rm -rf "+shellQuote(stagingDir)+" && mkdir -p "+shellQuote(stagingDir))
	}
	if err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStart)

	extractStart := b.now()
	if err := b.execShell(syncCtx, api, sandboxID, "tar -xzf "+shellQuote(remoteArchive)+" -C "+shellQuote(extractDir)); err != nil {
		return nil, 0, err
	}
	extractDuration := b.now().Sub(extractStart)

	replaceDuration := time.Duration(0)
	if stagingDir != "" {
		replaceStart := b.now()
		if err := b.replaceWorkspace(syncCtx, api, sandboxID, stagingDir, workdir); err != nil {
			return nil, 0, err
		}
		replaceDuration = b.now().Sub(replaceStart)
	}

	cleanupStart := b.now()
	cleanupRemote()
	cleanupPending = false
	cleanupDuration := b.now().Sub(cleanupStart)

	total := b.now().Sub(start)
	phases := []timingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "archive", Ms: archiveDuration.Milliseconds()},
		{Name: "upload", Ms: uploadDuration.Milliseconds()},
		{Name: "prepare", Ms: prepareDuration.Milliseconds()},
		{Name: "extract", Ms: extractDuration.Milliseconds()},
	}
	if stagingDir != "" {
		phases = append(phases, timingPhase{Name: "replace", Ms: replaceDuration.Milliseconds()})
	}
	phases = append(phases, timingPhase{Name: "cleanup", Ms: cleanupDuration.Milliseconds()})
	phases = append(phases, timingPhase{Name: "opencomputer_sync", Ms: total.Milliseconds()})
	return phases, total, nil
}

func checkOpenComputerSyncPreflight(manifest SyncManifest, cfg Config, force bool, stderr io.Writer) error {
	// OpenComputer uploads a full archive even when only a small dirty delta
	// exists, so guardrails must evaluate the full candidate.
	archiveManifest := manifest
	archiveManifest.Changed = nil
	archiveManifest.ChangedBytes = 0
	return checkSyncPreflight(archiveManifest, cfg, force, stderr)
}

func (b *openComputerBackend) ensureWorkspace(ctx context.Context, api *ocAPIClient, sandboxID, workdir string) error {
	// --no-sync must never apply sync.delete to a retained workspace.
	return b.execShell(ctx, api, sandboxID, "mkdir -p "+shellQuote(workdir))
}

func (b *openComputerBackend) replaceWorkspace(ctx context.Context, api *ocAPIClient, sandboxID, stagingDir, workdir string) error {
	backupDir := stagingDir + ".previous"
	command := "rm -rf " + shellQuote(backupDir) +
		" && if [ -e " + shellQuote(workdir) + " ]; then mv " + shellQuote(workdir) + " " + shellQuote(backupDir) + "; fi" +
		" && if mv " + shellQuote(stagingDir) + " " + shellQuote(workdir) +
		"; then exit 0" +
		"; else rc=$?; if [ -e " + shellQuote(backupDir) + " ]; then mv " + shellQuote(backupDir) + " " + shellQuote(workdir) +
		"; fi; exit \"$rc\"; fi"
	if err := b.execShell(ctx, api, sandboxID, command); err != nil {
		return err
	}
	if err := b.execShell(ctx, api, sandboxID, "rm -rf "+shellQuote(backupDir)); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: opencomputer previous workspace cleanup failed path=%s: %v\n", backupDir, err)
	}
	return nil
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
