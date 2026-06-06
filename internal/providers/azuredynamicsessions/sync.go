package azuredynamicsessions

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

func (b *azureDynamicSessionsBackend) syncWorkspace(ctx context.Context, client azureDynamicSessionsAPI, sessionID string, req RunRequest, workspace string) ([]timingPhase, time.Duration, error) {
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
	prepareStarted := b.now()
	if err := b.prepareWorkspace(syncCtx, client, sessionID, workspace, b.cfg.Sync.Delete); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStarted)
	archiveStarted := b.now()
	archive, err := createAzureDynamicSessionsSyncArchive(req.Repo, manifest)
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	archiveDuration := b.now().Sub(archiveStarted)
	uploadStarted := b.now()
	remoteArchive := azureDynamicSessionsRemoteArchivePath()
	if err := client.UploadFile(syncCtx, sessionID, archive.Name(), remoteArchive); err != nil {
		return nil, 0, providerError("upload archive", err)
	}
	if err := b.execShell(syncCtx, client, sessionID, azureDynamicSessionsExtractArchiveCommand(remoteArchive, workspace), io.Discard); err != nil {
		return nil, 0, err
	}
	uploadDuration := b.now().Sub(uploadStarted)
	total := b.now().Sub(start)
	return []timingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "prepare", Ms: prepareDuration.Milliseconds()},
		{Name: "archive", Ms: archiveDuration.Milliseconds()},
		{Name: "upload", Ms: uploadDuration.Milliseconds()},
		{Name: "azure_dynamic_sessions_sync", Ms: total.Milliseconds()},
	}, total, nil
}

func (b *azureDynamicSessionsBackend) prepareWorkspace(ctx context.Context, client azureDynamicSessionsAPI, sessionID, workspace string, reset bool) error {
	workspace, err := cleanAzureDynamicSessionsWorkspacePath(workspace)
	if err != nil {
		return err
	}
	command := "mkdir -p " + shellQuote(workspace)
	if reset {
		command = "rm -rf " + shellQuote(workspace) + " && " + command
	}
	return b.execShell(ctx, client, sessionID, command, io.Discard)
}

func (b *azureDynamicSessionsBackend) execShell(ctx context.Context, client azureDynamicSessionsAPI, sessionID, command string, stdout io.Writer) error {
	code, err := client.ExecStream(ctx, sessionID, azureDynamicSessionsExecRequest{
		Command:   command,
		Cwd:       "/",
		TimeoutMS: durationMillisecondsCeil(azureDynamicSessionsTimeout(b.cfg)),
	}, stdout, b.rt.Stderr)
	if err != nil {
		return fmt.Errorf("%s exec %q: %w", providerName, command, err)
	}
	if code != 0 {
		return exit(code, "%s exec %q exited %d", providerName, command, code)
	}
	return nil
}

func createAzureDynamicSessionsSyncArchive(repo Repo, manifest SyncManifest) (*os.File, error) {
	archive, err := os.CreateTemp("", "crabbox-azds-sync-*.tgz")
	if err != nil {
		return nil, fmt.Errorf("create sync archive temp file: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			name := archive.Name()
			_ = archive.Close()
			_ = os.Remove(name)
		}
	}()
	gz := gzip.NewWriter(archive)
	tw := tar.NewWriter(gz)
	for _, rel := range manifest.Files {
		if err := appendArchiveMember(tw, repo.Root, rel); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("create sync archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("create sync archive: %w", err)
	}
	if _, err := archive.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("rewind sync archive: %w", err)
	}
	keep = true
	return archive, nil
}

func appendArchiveMember(tw *tar.Writer, root, rel string) error {
	clean := path.Clean(filepath.ToSlash(rel))
	if clean == "." || clean != filepath.ToSlash(rel) || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return exit(6, "unsafe sync path %q", rel)
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	info, err := os.Lstat(full)
	if err != nil {
		return fmt.Errorf("stat sync path %s: %w", rel, err)
	}
	linkname := ""
	if info.Mode()&os.ModeSymlink != 0 {
		linkname, err = os.Readlink(full)
		if err != nil {
			return fmt.Errorf("read symlink %s: %w", rel, err)
		}
	}
	header, err := tar.FileInfoHeader(info, linkname)
	if err != nil {
		return fmt.Errorf("archive header %s: %w", rel, err)
	}
	header.Name = clean
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("archive header %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(full)
	if err != nil {
		return fmt.Errorf("open sync path %s: %w", rel, err)
	}
	defer file.Close()
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("archive path %s: %w", rel, err)
	}
	return nil
}

func azureDynamicSessionsWorkspace(cfg Config) (string, error) {
	return cleanAzureDynamicSessionsWorkspacePath(blank(strings.TrimSpace(cfg.AzureDynamicSessions.Workdir), "/workspace/crabbox"))
}

func cleanAzureDynamicSessionsWorkspacePath(workspace string) (string, error) {
	trimmed := strings.TrimSpace(workspace)
	if trimmed == "" {
		return "", exit(2, "%s workspace path is empty", providerName)
	}
	clean := path.Clean(trimmed)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "%s workspace path %q must resolve to an absolute path", providerName, workspace)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/mnt", "/mnt/data", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var", "/workspace":
		return "", exit(2, "%s workspace path %q is too broad; choose a dedicated subdirectory", providerName, clean)
	}
	return clean, nil
}

func buildAzureDynamicSessionsCommand(command []string, shellMode bool) (string, error) {
	if len(command) == 0 {
		return "", errors.New("missing command")
	}
	if shellMode {
		return strings.Join(command, " "), nil
	}
	if len(command) == 1 && shouldUseShell(command) {
		return command[0], nil
	}
	if shouldUseShell(command) || leadingEnvAssignment(command) {
		return shellScriptFromArgv(command), nil
	}
	return strings.Join(shellWords(command), " "), nil
}

func azureDynamicSessionsRemoteArchivePath() string {
	return path.Join("/tmp", "crabbox-azds-sync-"+time.Now().UTC().Format("20060102150405.000000000")+".tgz")
}

func azureDynamicSessionsExtractArchiveCommand(remoteArchive, workdir string) string {
	return strings.Join([]string{
		"tar -xzf " + shellQuote(remoteArchive) + " -C " + shellQuote(workdir),
		"status=$?",
		"rm -f " + shellQuote(remoteArchive),
		"cleanup=$?",
		`if [ "$status" -ne 0 ]; then exit "$status"; fi`,
		`exit "$cleanup"`,
	}, "; ")
}
