package e2b

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

func (b *e2bBackend) syncWorkspace(ctx context.Context, client e2bAPI, session e2bSession, req RunRequest, workspace string) ([]timingPhase, time.Duration, error) {
	workspace, err := cleanE2BWorkspacePath(workspace)
	if err != nil {
		return nil, 0, err
	}
	start := b.now()
	excludes, err := syncExcludes(req.Repo.Root, b.cfg)
	if err != nil {
		return nil, 0, err
	}
	manifestStarted := b.now()
	manifest, err := syncManifest(req.Repo.Root, excludes)
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
	if err := b.prepareWorkspace(ctx, client, session, workspace); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStarted)
	archiveStarted := b.now()
	archive, err := createE2BSyncArchive(ctx, req.Repo, manifest, b.rt.Stderr)
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	archiveDuration := b.now().Sub(archiveStarted)
	uploadStarted := b.now()
	if _, err := archive.Seek(0, 0); err != nil {
		return nil, 0, fmt.Errorf("e2b rewind archive: %w", err)
	}
	remoteArchive := path.Join("/tmp", "crabbox-"+e2bRandomSuffix()+".tgz")
	if err := client.UploadFile(ctx, session, remoteArchive, archive); err != nil {
		return nil, 0, e2bError("upload archive", err)
	}
	extract := strings.Join([]string{
		"tar -xzf " + shellQuote(remoteArchive) + " -C " + shellQuote(workspace),
		"rm -f " + shellQuote(remoteArchive),
	}, " && ")
	if err := b.execShell(ctx, client, session, extract, io.Discard); err != nil {
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
		{Name: "e2b_sync", Ms: total.Milliseconds()},
	}, total, nil
}

func (b *e2bBackend) prepareWorkspace(ctx context.Context, client e2bAPI, session e2bSession, workspace string) error {
	workspace, err := cleanE2BWorkspacePath(workspace)
	if err != nil {
		return err
	}
	command := "mkdir -p " + shellQuote(workspace)
	if b.cfg.Sync.Delete {
		command = "rm -rf " + shellQuote(workspace) + " && " + command
	}
	return b.execShell(ctx, client, session, command, io.Discard)
}

func cleanE2BWorkspacePath(workspace string) (string, error) {
	trimmed := strings.TrimSpace(workspace)
	if trimmed == "" {
		return "", exit(2, "e2b workspace path is empty")
	}
	clean := path.Clean(trimmed)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "e2b workspace path %q must resolve to an absolute path", workspace)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var":
		return "", exit(2, "e2b workspace path %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func (b *e2bBackend) execShell(ctx context.Context, client e2bAPI, session e2bSession, command string, stdout io.Writer) error {
	user, err := e2bProcessUser(b.cfg.E2B.User)
	if err != nil {
		return err
	}
	code, err := client.StartProcess(ctx, session, e2bProcessRequest{
		Command: command,
		User:    user,
		Timeout: b.cfg.TTL,
		Stdout:  stdout,
		Stderr:  b.rt.Stderr,
	})
	if err != nil {
		return fmt.Errorf("e2b exec %q: %w", command, err)
	}
	if code != 0 {
		return exit(code, "e2b exec %q exited %d", command, code)
	}
	return nil
}

func createE2BSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, stderr io.Writer) (*os.File, error) {
	var input bytes.Buffer
	input.Write(manifest.NUL())
	archive, err := os.CreateTemp("", "crabbox-e2b-sync-*.tgz")
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
	cmd := exec.CommandContext(ctx, "tar", "--no-xattrs", "-czf", "-", "-C", repo.Root, "--null", "-T", "-")
	cmd.Stdin = &input
	cmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	cmd.Stdout = archive
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, exit(6, "create sync archive: %v", err)
	}
	keep = true
	return archive, nil
}

func e2bRandomSuffix() string {
	return strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
}
