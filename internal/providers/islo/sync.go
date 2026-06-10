package islo

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	core "github.com/openclaw/crabbox/internal/cli"
)

type SyncManifest = core.SyncManifest

func rejectIsloSyncOptions(req RunRequest) error {
	if req.SyncOnly {
		return exit(2, "%s uses Islo archive sync; --sync-only is not supported", isloProvider)
	}
	if req.ChecksumSync {
		return exit(2, "%s uses Islo archive sync; --checksum is not supported", isloProvider)
	}
	if len(req.ArtifactGlobs) > 0 {
		return exit(2, "%s supports bounded single-file artifacts; --artifact-glob is not supported", isloProvider)
	}
	return nil
}

func (b *isloBackend) syncWorkspace(ctx context.Context, client isloAPI, name string, req RunRequest, user string) ([]timingPhase, time.Duration, error) {
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
	workspace, err := isloWorkspacePath(b.cfg)
	if err != nil {
		return nil, 0, err
	}
	prepareStarted := b.now()
	if err := b.prepareWorkspace(ctx, client, name, workspace, user); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStarted)
	archiveStarted := b.now()
	archive, err := createIsloSyncArchive(ctx, req.Repo, manifest, b.rt.Stderr)
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	archiveDuration := b.now().Sub(archiveStarted)
	uploadStarted := b.now()
	if _, err := archive.Seek(0, 0); err != nil {
		return nil, 0, fmt.Errorf("islo rewind archive: %w", err)
	}
	if err := client.UploadArchive(ctx, name, workspace, struct{ io.Reader }{archive}); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: islo archive API upload failed; falling back to exec upload: %v\n", err)
		if user != "" {
			if ownershipErr := b.restoreWorkspaceOwnership(ctx, client, name, workspace, user); ownershipErr != nil {
				return nil, 0, ownershipErr
			}
		}
		if _, seekErr := archive.Seek(0, 0); seekErr != nil {
			return nil, 0, fmt.Errorf("islo rewind archive for fallback: %w", seekErr)
		}
		if fallbackErr := b.uploadArchiveViaExec(ctx, client, name, workspace, archive, user); fallbackErr != nil {
			return nil, 0, fallbackErr
		}
	} else if user != "" {
		if err := b.restoreWorkspaceOwnership(ctx, client, name, workspace, user); err != nil {
			return nil, 0, err
		}
	}
	uploadDuration := b.now().Sub(uploadStarted)
	total := b.now().Sub(start)
	return []timingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "prepare", Ms: prepareDuration.Milliseconds()},
		{Name: "archive", Ms: archiveDuration.Milliseconds()},
		{Name: "upload", Ms: uploadDuration.Milliseconds()},
		{Name: "islo_sync", Ms: total.Milliseconds()},
	}, total, nil
}

func (b *isloBackend) prepareWorkspace(ctx context.Context, client isloAPI, name, workspace, user string) error {
	command := "mkdir -p " + shellQuote(workspace)
	if b.cfg.Sync.Delete {
		command = "rm -rf " + shellQuote(workspace) + " && " + command
	}
	return b.execShellAs(ctx, client, name, command, user, io.Discard)
}

func (b *isloBackend) repairWorkspaceOwnership(ctx context.Context, client isloAPI, name, workspace string) error {
	return b.execShellAs(ctx, client, name, isloWorkspaceOwnershipRepairCommand(workspace), isloAdminUser, io.Discard)
}

func isloWorkspaceOwnershipRepairCommand(workspace string) string {
	return strings.Join([]string{
		"set -e",
		"mkdir -p " + shellQuote(workspace),
		"chown -R " + shellQuote(isloWorkloadUser+":"+isloWorkloadUser) + " " + shellQuote(workspace),
	}, "\n")
}

func (b *isloBackend) restoreWorkspaceOwnership(ctx context.Context, client isloAPI, name, workspace, user string) error {
	command := "chown -R " + shellQuote(user+":"+user) + " " + shellQuote(workspace)
	return b.execShellAs(ctx, client, name, command, isloAdminUser, io.Discard)
}

func (b *isloBackend) uploadArchiveViaExec(ctx context.Context, client isloAPI, name, workspace string, archive io.Reader, user string) error {
	suffix := isloRandomSuffix()
	remoteB64 := path.Join("/tmp", "crabbox-"+suffix+".tgz.b64")
	remoteArchive := path.Join("/tmp", "crabbox-"+suffix+".tgz")
	cleanup := "rm -f " + shellQuote(remoteB64) + " " + shellQuote(remoteArchive)
	if err := b.execShellAs(ctx, client, name, cleanup, user, io.Discard); err != nil {
		return err
	}
	cleanupRemote := true
	defer func() {
		if cleanupRemote {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			_ = b.execShellAs(cleanupCtx, client, name, cleanup, user, io.Discard)
		}
	}()
	buf := make([]byte, 48*1024)
	for {
		n, readErr := archive.Read(buf)
		if n > 0 {
			chunk := base64.StdEncoding.EncodeToString(buf[:n])
			command := "printf %s " + shellQuote(chunk) + " >> " + shellQuote(remoteB64)
			if err := b.execShellAs(ctx, client, name, command, user, io.Discard); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("islo read archive for fallback upload: %w", readErr)
		}
	}
	if err := b.execShellAs(ctx, client, name, isloFallbackExtractCommand(remoteB64, remoteArchive, workspace), user, io.Discard); err != nil {
		return err
	}
	cleanupRemote = false
	return nil
}

func isloFallbackExtractCommand(remoteB64, remoteArchive, workspace string) string {
	extract := strings.Join([]string{
		"if base64 -d " + shellQuote(remoteB64) + " > " + shellQuote(remoteArchive) + " 2>/dev/null; then :; else base64 --decode " + shellQuote(remoteB64) + " > " + shellQuote(remoteArchive) + "; fi",
		"tar -xzf " + shellQuote(remoteArchive) + " -C " + shellQuote(workspace),
	}, " && ")
	cleanup := "rm -f " + shellQuote(remoteB64) + " " + shellQuote(remoteArchive)
	return extract + "; status=$?; " + cleanup + "; exit $status"
}

func (b *isloBackend) execShellAs(ctx context.Context, client isloAPI, name, command, user string, stdout io.Writer) error {
	req := &gosdk.ExecRequest{Command: []string{"bash", "-lc", command}}
	if user != "" {
		req.User = stringValue(user)
	}
	code, err := client.ExecStream(ctx, name, req, stdout, b.rt.Stderr)
	if err != nil {
		return fmt.Errorf("islo exec %q: %w", isloCommandForError(command), err)
	}
	if code != 0 {
		return exit(code, "islo exec %q exited %d", isloCommandForError(command), code)
	}
	return nil
}

func isloCommandForError(command string) string {
	if strings.HasPrefix(command, "printf %s ") && strings.Contains(command, " >> ") {
		return "append archive chunk"
	}
	return command
}

func createIsloSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, _ io.Writer) (*os.File, error) {
	return core.CreateSyncArchive(ctx, repo, manifest, "crabbox-islo-sync-*.tgz")
}

func isloWorkspacePath(cfg Config) (string, error) {
	workdir, err := isloRelativeWorkdir(cfg)
	if err != nil {
		return "", err
	}
	return path.Join("/workspace", workdir), nil
}

func isloRelativeWorkdir(cfg Config) (string, error) {
	workdir := strings.TrimSpace(cfg.Islo.Workdir)
	if workdir == "" {
		workdir = "crabbox"
	}
	if strings.HasPrefix(workdir, "/") {
		return "", exit(2, "islo workdir %q must be relative under /workspace", workdir)
	}
	workdir = path.Clean(workdir)
	if workdir == "." || workdir == ".." || strings.HasPrefix(workdir, "../") {
		return "", exit(2, "islo workdir %q escapes /workspace", workdir)
	}
	return workdir, nil
}
