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
		stagingDir = path.Join(path.Dir(workdir), "."+path.Base(workdir)+".crabbox-sync-"+randomSuffix())
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
		err = b.execShell(syncCtx, client, ready, "rm -rf "+shellQuote(stagingDir)+" && mkdir -p "+shellQuote(stagingDir))
	}
	if err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStart)

	uploadStart := b.now()
	if _, err := archive.Seek(0, 0); err != nil {
		return nil, 0, exit(6, "rewind sync archive: %v", err)
	}
	if err := client.Exec(syncCtx, podExecRequest{
		Namespace: b.cfg.AgentSandbox.Namespace,
		Pod:       ready.PodName,
		Container: b.cfg.AgentSandbox.Container,
		Command:   []string{"tar", "-xzf", "-", "-C", extractDir},
		Stdin:     archive,
		Stdout:    b.rt.Stdout,
		Stderr:    b.rt.Stderr,
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
	backupDir := stagingDir + ".previous"
	command := "rm -rf " + shellQuote(backupDir) +
		" && if [ -e " + shellQuote(workdir) + " ]; then mv " + shellQuote(workdir) + " " + shellQuote(backupDir) + "; fi" +
		" && if mv " + shellQuote(stagingDir) + " " + shellQuote(workdir) +
		"; then exit 0" +
		"; else rc=$?; if [ -e " + shellQuote(backupDir) + " ]; then mv " + shellQuote(backupDir) + " " + shellQuote(workdir) +
		"; fi; exit \"$rc\"; fi"
	if err := b.execShell(ctx, client, ready, command); err != nil {
		return err
	}
	if err := b.execShell(ctx, client, ready, "rm -rf "+shellQuote(backupDir)); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: agent-sandbox previous workspace cleanup failed path=%s: %v\n", backupDir, err)
	}
	return nil
}

func randomSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
