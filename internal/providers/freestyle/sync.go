package freestyle

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type SyncManifest = core.SyncManifest

const maxFreestyleArchiveUploadBytes = 64 << 20

func (b *freestyleBackend) syncWorkspace(ctx context.Context, client freestyleAPI, name string, req RunRequest) ([]timingPhase, time.Duration, error) {
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
	manifestStarted := b.now()
	manifest, err := syncManifest(req.Repo.Root, excludes, b.cfg.Sync.Includes)
	if err != nil {
		return nil, 0, exit(6, "build sync file list: %v", err)
	}
	manifestDuration := b.now().Sub(manifestStarted)
	preflightStarted := b.now()
	if err := checkSyncPreflight(manifest, b.cfg, req.ForceSyncLarge, b.rt.Stderr); err != nil {
		return nil, 0, err
	}
	preflightDuration := b.now().Sub(preflightStarted)
	archiveStarted := b.now()
	archive, err := createFreestyleSyncArchive(syncCtx, req.Repo, manifest, b.rt.Stderr)
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if _, err := archive.Seek(0, 0); err != nil {
		return nil, 0, fmt.Errorf("freestyle rewind archive: %w", err)
	}
	archiveData, err := readFreestyleArchiveForUpload(archive)
	if err != nil {
		return nil, 0, fmt.Errorf("freestyle read archive: %w", err)
	}
	archiveDuration := b.now().Sub(archiveStarted)
	workspace, err := freestyleWorkspacePath(b.cfg)
	if err != nil {
		return nil, 0, err
	}
	extractDir := workspace
	stagingDir := ""
	if b.cfg.Sync.Delete {
		stagingDir = path.Join(path.Dir(workspace), "."+path.Base(workspace)+".crabbox-sync-"+freestyleRandomSuffix())
		extractDir = stagingDir
	}
	cleanupStaging := stagingDir != ""
	defer func() {
		if cleanupStaging {
			b.cleanupFreestyleWorkspace(client, name, stagingDir)
		}
	}()
	prepareStarted := b.now()
	if err := b.prepareWorkspace(syncCtx, client, name, extractDir, stagingDir != ""); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStarted)
	uploadStarted := b.now()
	b64Content := base64.StdEncoding.EncodeToString(archiveData)
	suffix := freestyleRandomSuffix()
	remoteArchive := "/tmp/crabbox-" + suffix + ".tgz"
	if err := client.WriteFile(syncCtx, name, remoteArchive, b64Content, "base64"); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: freestyle file API upload failed; falling back to exec upload: %v\n", err)
		b.cleanupFreestyleUpload(client, name, remoteArchive)
		if fallbackErr := b.uploadArchiveViaExec(syncCtx, client, name, extractDir, archiveData); fallbackErr != nil {
			return nil, 0, fallbackErr
		}
	} else {
		if err := b.extractFreestyleArchive(syncCtx, client, name, remoteArchive, extractDir); err != nil {
			return nil, 0, err
		}
	}
	uploadDuration := b.now().Sub(uploadStarted)
	replaceDuration := time.Duration(0)
	if stagingDir != "" {
		replaceStarted := b.now()
		if err := b.replaceFreestyleWorkspace(syncCtx, client, name, stagingDir, workspace); err != nil {
			return nil, 0, err
		}
		cleanupStaging = false
		replaceDuration = b.now().Sub(replaceStarted)
	}
	total := b.now().Sub(start)
	phases := []timingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "archive", Ms: archiveDuration.Milliseconds()},
		{Name: "prepare", Ms: prepareDuration.Milliseconds()},
		{Name: "upload", Ms: uploadDuration.Milliseconds()},
	}
	if stagingDir != "" {
		phases = append(phases, timingPhase{Name: "replace", Ms: replaceDuration.Milliseconds()})
	}
	phases = append(phases, timingPhase{Name: "freestyle_sync", Ms: total.Milliseconds()})
	return phases, total, nil
}

func (b *freestyleBackend) prepareWorkspace(ctx context.Context, client freestyleAPI, name, workspace string, delete bool) error {
	command := "mkdir -p " + shellQuote(workspace)
	if delete {
		command = "rm -rf " + shellQuote(workspace) + " && " + command
	}
	return b.execShell(ctx, client, name, command)
}

func (b *freestyleBackend) uploadArchiveViaExec(ctx context.Context, client freestyleAPI, name, workspace string, archiveData []byte) error {
	suffix := freestyleRandomSuffix()
	remoteB64 := "/tmp/crabbox-" + suffix + ".tgz.b64"
	remoteArchive := "/tmp/crabbox-" + suffix + ".tgz"
	cleanupCommand := "rm -f " + shellQuote(remoteB64) + " " + shellQuote(remoteArchive)
	if err := b.execShell(ctx, client, name, cleanupCommand); err != nil {
		return err
	}
	cleanupNeeded := true
	defer func() {
		if !cleanupNeeded {
			return
		}
		b.cleanupFreestyleUpload(client, name, remoteB64, remoteArchive)
	}()
	buf := archiveData
	chunkSize := 48 * 1024
	chunkCount := (len(buf) + chunkSize - 1) / chunkSize
	for i := 0; i < len(buf); i += chunkSize {
		end := i + chunkSize
		if end > len(buf) {
			end = len(buf)
		}
		chunk := base64.StdEncoding.EncodeToString(buf[i:end])
		command := "printf %s " + shellQuote(chunk) + " >> " + shellQuote(remoteB64)
		action := fmt.Sprintf("upload archive chunk %d/%d", i/chunkSize+1, chunkCount)
		if err := b.execShellRedacted(ctx, client, name, command, action); err != nil {
			return err
		}
	}
	if err := b.execShell(ctx, client, name, freestyleFallbackExtractCommand(remoteB64, remoteArchive, workspace)); err != nil {
		return err
	}
	cleanupNeeded = false
	return nil
}

func (b *freestyleBackend) extractFreestyleArchive(ctx context.Context, client freestyleAPI, name, remoteArchive, workspace string) error {
	cleanupNeeded := true
	defer func() {
		if cleanupNeeded {
			b.cleanupFreestyleUpload(client, name, remoteArchive)
		}
	}()
	if err := b.execShell(ctx, client, name, freestyleDirectExtractCommand(remoteArchive, workspace)); err != nil {
		return err
	}
	cleanupNeeded = false
	return nil
}

func (b *freestyleBackend) cleanupFreestyleUpload(client freestyleAPI, name string, paths ...string) {
	if len(paths) == 0 {
		return
	}
	quoted := make([]string, 0, len(paths))
	for _, remotePath := range paths {
		quoted = append(quoted, shellQuote(remotePath))
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), freestyleCleanupTimeout)
	defer cancel()
	if err := b.execShell(cleanupCtx, client, name, "rm -f "+strings.Join(quoted, " ")); err != nil && b.rt.Stderr != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: freestyle upload cleanup failed for %s: %v\n", name, err)
	}
}

func (b *freestyleBackend) cleanupFreestyleWorkspace(client freestyleAPI, name, workspace string) {
	if strings.TrimSpace(workspace) == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), freestyleCleanupTimeout)
	defer cancel()
	if err := b.execShell(cleanupCtx, client, name, "rm -rf "+shellQuote(workspace)); err != nil && b.rt.Stderr != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: freestyle staging cleanup failed for %s: %v\n", name, err)
	}
}

func (b *freestyleBackend) replaceFreestyleWorkspace(ctx context.Context, client freestyleAPI, name, stagingDir, workspace string) error {
	backupDir := stagingDir + ".previous"
	command := "rm -rf " + shellQuote(backupDir) +
		" && if [ -e " + shellQuote(workspace) + " ]; then mv " + shellQuote(workspace) + " " + shellQuote(backupDir) + "; fi" +
		" && if mv " + shellQuote(stagingDir) + " " + shellQuote(workspace) +
		"; then exit 0" +
		"; else rc=$?; if [ -e " + shellQuote(backupDir) + " ]; then mv " + shellQuote(backupDir) + " " + shellQuote(workspace) +
		"; fi; exit \"$rc\"; fi"
	if err := b.execShell(ctx, client, name, command); err != nil {
		return err
	}
	if err := b.execShell(ctx, client, name, "rm -rf "+shellQuote(backupDir)); err != nil && b.rt.Stderr != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: freestyle previous workspace cleanup failed path=%s: %v\n", backupDir, err)
	}
	return nil
}

func readFreestyleArchiveForUpload(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, maxFreestyleArchiveUploadBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxFreestyleArchiveUploadBytes {
		return nil, exit(6, "freestyle sync archive exceeds %d bytes after compression; narrow sync.include/excludes or split the run", maxFreestyleArchiveUploadBytes)
	}
	return data, nil
}

func freestyleDirectExtractCommand(remoteArchive, workspace string) string {
	extract := "tar -xzf " + shellQuote(remoteArchive) + " -C " + shellQuote(workspace)
	cleanup := "rm -f " + shellQuote(remoteArchive)
	return extract + "; status=$?; " + cleanup + "; exit $status"
}

func freestyleFallbackExtractCommand(remoteB64, remoteArchive, workspace string) string {
	extract := strings.Join([]string{
		"if base64 -d " + shellQuote(remoteB64) + " > " + shellQuote(remoteArchive) + " 2>/dev/null; then :; else base64 --decode " + shellQuote(remoteB64) + " > " + shellQuote(remoteArchive) + "; fi",
		"tar -xzf " + shellQuote(remoteArchive) + " -C " + shellQuote(workspace),
	}, " && ")
	cleanup := "rm -f " + shellQuote(remoteB64) + " " + shellQuote(remoteArchive)
	return extract + "; status=$?; " + cleanup + "; exit $status"
}

func (b *freestyleBackend) execShell(ctx context.Context, client freestyleAPI, name, command string) error {
	code, err := client.Exec(ctx, name, "bash -lc "+shellQuote(command), io.Discard, b.rt.Stderr)
	if err != nil {
		return fmt.Errorf("freestyle exec %q: %w", command, err)
	}
	if code != 0 {
		return exit(code, "freestyle exec %q exited %d", command, code)
	}
	return nil
}

func (b *freestyleBackend) execShellRedacted(ctx context.Context, client freestyleAPI, name, command, action string) error {
	code, err := client.Exec(ctx, name, "bash -lc "+shellQuote(command), io.Discard, b.rt.Stderr)
	if err != nil {
		return fmt.Errorf("freestyle %s: %w", action, err)
	}
	if code != 0 {
		return exit(code, "freestyle %s exited %d", action, code)
	}
	return nil
}

func createFreestyleSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, _ io.Writer) (*os.File, error) {
	return core.CreateSyncArchive(ctx, repo, manifest, "crabbox-freestyle-sync-*.tgz")
}

func freestyleWorkspacePath(cfg Config) (string, error) {
	workdir, err := freestyleRelativeWorkdir(cfg)
	if err != nil {
		return "", err
	}
	return path.Join("/workspace", workdir), nil
}

func freestyleRelativeWorkdir(cfg Config) (string, error) {
	workdir := strings.TrimSpace(cfg.Freestyle.Workdir)
	if workdir == "" {
		workdir = "crabbox"
	}
	if strings.HasPrefix(workdir, "/") {
		return "", exit(2, "freestyle workdir %q must be relative under /workspace", workdir)
	}
	workdir = path.Clean(workdir)
	if workdir == "." || workdir == ".." || strings.HasPrefix(workdir, "../") {
		return "", exit(2, "freestyle workdir %q escapes /workspace", workdir)
	}
	return workdir, nil
}
