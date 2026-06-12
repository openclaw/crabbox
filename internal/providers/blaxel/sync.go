package blaxel

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

func (b *backend) syncWorkspace(ctx context.Context, client Client, sandboxID string, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
	start := now(b.rt)
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
	manifestStarted := now(b.rt)
	manifest, err := syncManifest(req.Repo.Root, excludes, b.cfg.Sync.Includes)
	if err != nil {
		return nil, 0, exit(6, "build sync file list: %v", err)
	}
	manifestDuration := now(b.rt).Sub(manifestStarted)

	preflightStarted := now(b.rt)
	if err := checkBlaxelSyncPreflight(manifest, b.cfg, req.ForceSyncLarge, b.rt.Stderr); err != nil {
		return nil, 0, err
	}
	preflightDuration := now(b.rt).Sub(preflightStarted)

	archiveStarted := now(b.rt)
	archive, err := createBlaxelSyncArchive(syncCtx, req.Repo, manifest)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	archiveDuration := now(b.rt).Sub(archiveStarted)

	remoteArchive := path.Join("/tmp", "crabbox-blaxel-sync-"+randomSuffix()+".tgz")
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
		_ = b.execShell(cleanupCtx, client, sandboxID, command)
	}
	cleanupPending := true
	defer func() {
		if cleanupPending {
			cleanupRemote()
		}
	}()

	uploadStarted := now(b.rt)
	if _, err := archive.Seek(0, 0); err != nil {
		return nil, 0, exit(6, "rewind sync archive: %v", err)
	}
	if err := client.UploadFile(syncCtx, sandboxID, remoteArchive, archive); err != nil {
		return nil, 0, fmt.Errorf("blaxel upload archive: %w", err)
	}
	uploadDuration := now(b.rt).Sub(uploadStarted)

	prepareStarted := now(b.rt)
	if stagingDir == "" {
		err = b.ensureWorkspace(syncCtx, client, sandboxID, workdir)
	} else {
		err = b.execShell(syncCtx, client, sandboxID, "rm -rf "+shellQuote(stagingDir)+" && mkdir -p "+shellQuote(stagingDir))
	}
	if err != nil {
		return nil, 0, err
	}
	prepareDuration := now(b.rt).Sub(prepareStarted)

	extractStarted := now(b.rt)
	if err := b.execShell(syncCtx, client, sandboxID, "tar -xzf "+shellQuote(remoteArchive)+" -C "+shellQuote(extractDir)); err != nil {
		return nil, 0, err
	}
	extractDuration := now(b.rt).Sub(extractStarted)

	replaceDuration := time.Duration(0)
	if stagingDir != "" {
		replaceStarted := now(b.rt)
		if err := b.replaceWorkspace(syncCtx, client, sandboxID, stagingDir, workdir); err != nil {
			return nil, 0, err
		}
		replaceDuration = now(b.rt).Sub(replaceStarted)
	}

	cleanupStarted := now(b.rt)
	cleanupRemote()
	cleanupPending = false
	cleanupDuration := now(b.rt).Sub(cleanupStarted)

	total := now(b.rt).Sub(start)
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
	phases = append(phases, timingPhase{Name: "blaxel_sync", Ms: total.Milliseconds()})
	return phases, total, nil
}

func checkBlaxelSyncPreflight(manifest SyncManifest, cfg Config, force bool, writer io.Writer) error {
	if writer == nil {
		writer = os.Stderr
	}
	archiveManifest := manifest
	archiveManifest.Changed = nil
	archiveManifest.ChangedBytes = 0
	return checkSyncPreflight(archiveManifest, cfg, force, writer)
}

func (b *backend) ensureWorkspace(ctx context.Context, client Client, sandboxID, workdir string) error {
	return b.execShell(ctx, client, sandboxID, "mkdir -p "+shellQuote(workdir))
}

func (b *backend) replaceWorkspace(ctx context.Context, client Client, sandboxID, stagingDir, workdir string) error {
	backupDir := stagingDir + ".previous"
	command := "rm -rf " + shellQuote(backupDir) +
		" && if [ -e " + shellQuote(workdir) + " ]; then mv " + shellQuote(workdir) + " " + shellQuote(backupDir) + "; fi" +
		" && if mv " + shellQuote(stagingDir) + " " + shellQuote(workdir) +
		"; then exit 0" +
		"; else rc=$?; if [ -e " + shellQuote(backupDir) + " ]; then mv " + shellQuote(backupDir) + " " + shellQuote(workdir) +
		"; fi; exit \"$rc\"; fi"
	if err := b.execShell(ctx, client, sandboxID, command); err != nil {
		return err
	}
	if err := b.execShell(ctx, client, sandboxID, "rm -rf "+shellQuote(backupDir)); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: blaxel previous workspace cleanup failed path=%s: %v\n", backupDir, err)
	}
	return nil
}

func (b *backend) execShell(ctx context.Context, client Client, sandboxID, command string) error {
	res, err := client.ExecuteProcess(ctx, sandboxID, ExecuteProcessRequest{
		Command:     "bash",
		Args:        []string{"-lc", command},
		TimeoutSecs: b.execTimeoutSecs(),
	})
	if err != nil {
		return err
	}
	res, err = b.waitProcess(ctx, client, sandboxID, res)
	if err != nil {
		return err
	}
	if res.ExitCode != nil && *res.ExitCode == 0 {
		return nil
	}
	logs, logErr := client.GetProcessLogs(ctx, sandboxID, res.ID)
	if logErr != nil {
		return logErr
	}
	code := 1
	if res.ExitCode != nil {
		code = *res.ExitCode
	}
	return exit(code, "blaxel exec %q exited %d: %s", command, code, strings.TrimSpace(logs.Stderr))
}

func createBlaxelSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest) (*os.File, error) {
	return createPortableSyncArchive(ctx, repo, manifest, "crabbox-blaxel-sync-*.tgz")
}
