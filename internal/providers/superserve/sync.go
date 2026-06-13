package superserve

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

func (b *backend) syncWorkspace(ctx context.Context, api superserveClient, access *sandboxAccess, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
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
	if err := checkSuperserveSyncPreflight(manifest, b.cfg, req.ForceSyncLarge, b.rt.Stderr); err != nil {
		return nil, 0, err
	}
	preflightDuration := b.now().Sub(preflightStart)

	archiveStart := b.now()
	archive, err := createSuperserveSyncArchive(syncCtx, req.Repo, manifest)
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
		_ = b.execShell(cleanupCtx, api, access, command)
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
	if err := api.UploadFile(syncCtx, access, remoteArchive, archive); err != nil {
		return nil, 0, err
	}
	uploadDuration := b.now().Sub(uploadStart)

	prepareStart := b.now()
	if stagingDir == "" {
		err = b.ensureWorkspace(syncCtx, api, access, workdir)
	} else {
		err = b.execShell(syncCtx, api, access, "rm -rf "+shellQuote(stagingDir)+" && mkdir -p "+shellQuote(stagingDir))
	}
	if err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStart)

	extractStart := b.now()
	if err := b.execShell(syncCtx, api, access, "tar -xzf "+shellQuote(remoteArchive)+" -C "+shellQuote(extractDir)); err != nil {
		return nil, 0, err
	}
	extractDuration := b.now().Sub(extractStart)

	replaceDuration := time.Duration(0)
	if stagingDir != "" {
		replaceStart := b.now()
		if err := b.replaceWorkspace(syncCtx, api, access, stagingDir, workdir); err != nil {
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
	phases = append(phases, timingPhase{Name: "superserve_sync", Ms: total.Milliseconds()})
	return phases, total, nil
}

func checkSuperserveSyncPreflight(manifest SyncManifest, cfg Config, force bool, stderr io.Writer) error {
	archiveManifest := manifest
	archiveManifest.Changed = nil
	archiveManifest.ChangedBytes = 0
	return checkSyncPreflight(archiveManifest, cfg, force, stderr)
}

func (b *backend) ensureWorkspace(ctx context.Context, api superserveClient, access *sandboxAccess, workdir string) error {
	return b.execShell(ctx, api, access, "mkdir -p "+shellQuote(workdir))
}

func (b *backend) replaceWorkspace(ctx context.Context, api superserveClient, access *sandboxAccess, stagingDir, workdir string) error {
	backupDir := stagingDir + ".previous"
	command := "rm -rf " + shellQuote(backupDir) +
		" && if [ -e " + shellQuote(workdir) + " ]; then mv " + shellQuote(workdir) + " " + shellQuote(backupDir) + "; fi" +
		" && if mv " + shellQuote(stagingDir) + " " + shellQuote(workdir) +
		"; then exit 0" +
		"; else rc=$?; if [ -e " + shellQuote(backupDir) + " ]; then mv " + shellQuote(backupDir) + " " + shellQuote(workdir) +
		"; fi; exit \"$rc\"; fi"
	if err := b.execShell(ctx, api, access, command); err != nil {
		return err
	}
	if err := b.execShell(ctx, api, access, "rm -rf "+shellQuote(backupDir)); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: superserve previous workspace cleanup failed path=%s: %v\n", backupDir, err)
	}
	return nil
}

func (b *backend) execShell(ctx context.Context, api superserveClient, access *sandboxAccess, command string) error {
	res, err := api.Exec(ctx, access, execRequest{
		Command:     "sh -lc " + shellQuote(command),
		TimeoutSecs: b.execTimeoutSecs(),
	}, io.Discard, io.Discard)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return exit(res.ExitCode, "superserve exec %q exited %d: %s", command, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func createSuperserveSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest) (*os.File, error) {
	return createPortableSyncArchive(ctx, repo, manifest, "crabbox-superserve-sync-*.tgz")
}
