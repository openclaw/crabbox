package agentsandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"time"
)

func (b *backend) syncWorkspace(ctx context.Context, client kubernetesClient, ready sandboxReadiness, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
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
	if err := checkAgentSandboxSyncPreflight(manifest, b.cfg, req.ForceSyncLarge, b.rt.Stderr); err != nil {
		return nil, 0, err
	}
	preflightDuration := b.now().Sub(preflightStart)

	archiveStart := b.now()
	archive, err := createPortableSyncArchive(syncCtx, req.Repo, manifest, "crabbox-agent-sandbox-sync-*.tgz")
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	archiveDuration := b.now().Sub(archiveStart)

	extractDir := workdir
	stagingDir := ""
	if b.cfg.Sync.Delete {
		stagingDir = path.Join(workdir, ".crabbox-sync-"+randomSuffix())
		extractDir = stagingDir
	}
	cleanupPending := true
	cleanupRemote := func() {
		if !cleanupPending {
			return
		}
		cleanupCtx, cleanupCancel := b.cleanupContext(ctx)
		defer cleanupCancel()
		if stagingDir != "" {
			_ = b.execShell(cleanupCtx, client, ready, "rm -rf "+shellQuote(stagingDir)+" 2>/dev/null || true")
		}
	}
	defer cleanupRemote()

	prepareStart := b.now()
	if stagingDir == "" {
		err = b.execShell(syncCtx, client, ready, "mkdir -p "+shellQuote(workdir))
	} else {
		err = b.execShell(syncCtx, client, ready, "mkdir -p "+shellQuote(workdir)+" && rm -rf "+shellQuote(stagingDir)+" && mkdir -p "+shellQuote(stagingDir))
	}
	if err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStart)

	uploadStart := b.now()
	if _, err := archive.Seek(0, 0); err != nil {
		return nil, 0, exit(6, "rewind sync archive: %v", err)
	}
	if err := b.execPod(syncCtx, client, ready, podExecRequest{
		Command: []string{"tar", "-xzf", "-", "-C", extractDir},
		Stdin:   archive,
		Stdout:  b.rt.Stdout,
		Stderr:  b.rt.Stderr,
	}); err != nil {
		if code, ok := remoteExitStatus(err); ok {
			return nil, 0, exit(code, "agent-sandbox tar extract exited %d", code)
		}
		return nil, 0, err
	}
	uploadDuration := b.now().Sub(uploadStart)

	replaceDuration := time.Duration(0)
	if stagingDir != "" {
		replaceStart := b.now()
		if err := b.replaceWorkspace(syncCtx, client, ready, stagingDir, workdir); err != nil {
			return nil, 0, err
		}
		replaceDuration = b.now().Sub(replaceStart)
	}

	cleanupStart := b.now()
	cleanupPending = false
	cleanupDuration := b.now().Sub(cleanupStart)
	total := b.now().Sub(start)
	phases := []timingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "archive", Ms: archiveDuration.Milliseconds()},
		{Name: "prepare", Ms: prepareDuration.Milliseconds()},
		{Name: "upload_extract", Ms: uploadDuration.Milliseconds()},
	}
	if stagingDir != "" {
		phases = append(phases, timingPhase{Name: "replace", Ms: replaceDuration.Milliseconds()})
	}
	phases = append(phases, timingPhase{Name: "cleanup", Ms: cleanupDuration.Milliseconds()})
	phases = append(phases, timingPhase{Name: "agent_sandbox_sync", Ms: total.Milliseconds()})
	return phases, total, nil
}

func checkAgentSandboxSyncPreflight(manifest SyncManifest, cfg Config, force bool, stderr io.Writer) error {
	archiveManifest := manifest
	archiveManifest.Changed = nil
	archiveManifest.ChangedBytes = 0
	return checkSyncPreflight(archiveManifest, cfg, force, stderr)
}

func (b *backend) replaceWorkspace(ctx context.Context, client kubernetesClient, ready sandboxReadiness, stagingDir, workdir string) error {
	command := agentSandboxMountReplaceCommand(stagingDir, workdir)
	if err := b.execShell(ctx, client, ready, "bash -lc "+shellQuote(command)); err != nil {
		return err
	}
	backupDir := agentSandboxBackupDir(stagingDir, workdir)
	cleanupCtx, cleanupCancel := b.cleanupContext(ctx)
	defer cleanupCancel()
	if err := b.execShell(cleanupCtx, client, ready, "rm -rf "+shellQuote(backupDir)+" "+shellQuote(stagingDir)); err != nil {
		return fmt.Errorf("agent-sandbox replaced workspace but could not remove backup %s: %w", backupDir, err)
	}
	return nil
}

func agentSandboxMountReplaceCommand(stagingDir, workdir string) string {
	backupDir := agentSandboxBackupDir(stagingDir, workdir)
	workdirGlob := shellQuote(workdir) + "/*"
	backupGlob := shellQuote(backupDir) + "/*"
	stagingGlob := shellQuote(stagingDir) + "/*"
	rollback := "rollback() { original_rc=$?; trap - EXIT HUP INT TERM; rollback_rc=0; " +
		"if [ \"$copy_started\" -eq 1 ]; then for entry in " + workdirGlob + "; do " +
		"if [ \"$entry\" != " + shellQuote(backupDir) + " ] && [ \"$entry\" != " + shellQuote(stagingDir) + " ]; then rm -rf -- \"$entry\" || rollback_rc=$?; fi; done; fi; " +
		"if [ \"$rollback_rc\" -eq 0 ]; then for entry in " + backupGlob + "; do " +
		"mv -- \"$entry\" " + shellQuote(workdir+"/") + " || { rollback_rc=$?; break; }; done; fi; " +
		"if [ \"$rollback_rc\" -eq 0 ]; then rmdir " + shellQuote(backupDir) + " || rollback_rc=$?; fi; " +
		"if [ \"$rollback_rc\" -ne 0 ]; then exit \"$rollback_rc\"; fi; exit \"$original_rc\"; }"
	return "shopt -s dotglob nullglob; copy_started=0; " + rollback +
		"; mkdir -p " + shellQuote(workdir) +
		" && rm -rf " + shellQuote(backupDir) +
		" && mkdir -p " + shellQuote(backupDir) +
		" && trap rollback EXIT HUP INT TERM" +
		" && for entry in " + workdirGlob + "; do " +
		"if [ \"$entry\" != " + shellQuote(backupDir) + " ] && [ \"$entry\" != " + shellQuote(stagingDir) + " ]; then mv -- \"$entry\" " + shellQuote(backupDir+"/") + " || exit 1; fi; done" +
		" && copy_started=1" +
		" && for entry in " + stagingGlob + "; do cp -a -- \"$entry\" " + shellQuote(workdir+"/") + " || exit 1; done" +
		" && trap - EXIT HUP INT TERM"
}

func agentSandboxBackupDir(stagingDir, workdir string) string {
	return path.Join(workdir, path.Base(stagingDir)+".previous")
}

func randomSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
