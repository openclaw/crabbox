package cubesandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

func (b *cubesandboxBackend) syncWorkspace(ctx context.Context, client cubesandboxAPI, session cubesandboxSession, req RunRequest, workspace string) ([]timingPhase, time.Duration, error) {
	workspace, err := cleanCubeSandboxWorkspacePath(workspace)
	if err != nil {
		return nil, 0, err
	}
	start := b.now()
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
	if err := b.prepareWorkspace(ctx, client, session, workspace, b.cfg.Sync.Delete); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStarted)
	archiveStarted := b.now()
	archive, err := createCubeSandboxSyncArchive(ctx, req.Repo, manifest, b.rt.Stderr)
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	archiveDuration := b.now().Sub(archiveStarted)
	uploadStarted := b.now()
	if _, err := archive.Seek(0, 0); err != nil {
		return nil, 0, fmt.Errorf("cubesandbox rewind archive: %w", err)
	}
	remoteArchive := path.Join("/tmp", "crabbox-"+cubesandboxRandomSuffix()+".tgz")
	if err := client.UploadFile(ctx, session, remoteArchive, archive); err != nil {
		return nil, 0, cubesandboxError("upload archive", err)
	}
	extract := strings.Join([]string{
		"tar -xzf " + shellQuote(remoteArchive) + " -C " + shellQuote(workspace),
		"rm -f " + shellQuote(remoteArchive),
	}, " && ")
	if err := b.execShell(ctx, client, session, extract, io.Discard); err != nil {
		_ = b.execShell(context.Background(), client, session, "rm -f "+shellQuote(remoteArchive), io.Discard)
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
		{Name: "cubesandbox_sync", Ms: total.Milliseconds()},
	}, total, nil
}

func (b *cubesandboxBackend) prepareWorkspace(ctx context.Context, client cubesandboxAPI, session cubesandboxSession, workspace string, deleteExisting bool) error {
	workspace, err := cleanCubeSandboxWorkspacePath(workspace)
	if err != nil {
		return err
	}
	command := "mkdir -p " + shellQuote(workspace)
	if deleteExisting {
		command = "rm -rf " + shellQuote(workspace) + " && " + command
	}
	return b.execShell(ctx, client, session, command, io.Discard)
}

func cleanCubeSandboxWorkspacePath(workspace string) (string, error) {
	trimmed := strings.TrimSpace(workspace)
	if trimmed == "" {
		return "", exit(2, "cubesandbox workspace path is empty")
	}
	clean := path.Clean(trimmed)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "cubesandbox workspace path %q must resolve to an absolute path", workspace)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var":
		return "", exit(2, "cubesandbox workspace path %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func (b *cubesandboxBackend) execShell(ctx context.Context, client cubesandboxAPI, session cubesandboxSession, command string, stdout io.Writer) error {
	user, err := cubesandboxProcessUser(b.cfg.CubeSandbox.User)
	if err != nil {
		return err
	}
	code, err := client.StartProcess(ctx, session, cubesandboxProcessRequest{
		Command: command,
		User:    user,
		Timeout: b.cfg.TTL,
		Stdout:  stdout,
		Stderr:  b.rt.Stderr,
	})
	if err != nil {
		return fmt.Errorf("cubesandbox exec %q: %w", command, err)
	}
	if code != 0 {
		return exit(code, "cubesandbox exec %q exited %d", command, code)
	}
	return nil
}

func createCubeSandboxSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, _ io.Writer) (*os.File, error) {
	return createPortableSyncArchive(ctx, repo, manifest, "crabbox-cubesandbox-sync-*.tgz")
}

func cubesandboxRandomSuffix() string {
	return strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
}
