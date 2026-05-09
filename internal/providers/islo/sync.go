package islo

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	return nil
}

func (b *isloBackend) syncWorkspace(ctx context.Context, client isloAPI, name string, req RunRequest) ([]timingPhase, time.Duration, error) {
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
	workspace, err := isloWorkspacePath(b.cfg)
	if err != nil {
		return nil, 0, err
	}
	prepareStarted := b.now()
	if err := b.prepareWorkspace(ctx, client, name, workspace); err != nil {
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
		if _, seekErr := archive.Seek(0, 0); seekErr != nil {
			return nil, 0, fmt.Errorf("islo rewind archive for fallback: %w", seekErr)
		}
		if fallbackErr := b.uploadArchiveViaExec(ctx, client, name, workspace, archive); fallbackErr != nil {
			return nil, 0, fallbackErr
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

func (b *isloBackend) prepareWorkspace(ctx context.Context, client isloAPI, name, workspace string) error {
	command := "mkdir -p " + shellQuote(workspace)
	if b.cfg.Sync.Delete {
		command = "rm -rf " + shellQuote(workspace) + " && " + command
	}
	return b.execShell(ctx, client, name, command, io.Discard)
}

func (b *isloBackend) uploadArchiveViaExec(ctx context.Context, client isloAPI, name, workspace string, archive io.Reader) error {
	suffix := isloRandomSuffix()
	remoteB64 := path.Join("/tmp", "crabbox-"+suffix+".tgz.b64")
	remoteArchive := path.Join("/tmp", "crabbox-"+suffix+".tgz")
	if err := b.execShell(ctx, client, name, "rm -f "+shellQuote(remoteB64)+" "+shellQuote(remoteArchive), io.Discard); err != nil {
		return err
	}
	buf := make([]byte, 48*1024)
	for {
		n, readErr := archive.Read(buf)
		if n > 0 {
			chunk := base64.StdEncoding.EncodeToString(buf[:n])
			command := "printf %s " + shellQuote(chunk) + " >> " + shellQuote(remoteB64)
			if err := b.execShell(ctx, client, name, command, io.Discard); err != nil {
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
	extract := strings.Join([]string{
		"if base64 -d " + shellQuote(remoteB64) + " > " + shellQuote(remoteArchive) + " 2>/dev/null; then :; else base64 --decode " + shellQuote(remoteB64) + " > " + shellQuote(remoteArchive) + "; fi",
		"tar -xzf " + shellQuote(remoteArchive) + " -C " + shellQuote(workspace),
		"rm -f " + shellQuote(remoteB64) + " " + shellQuote(remoteArchive),
	}, " && ")
	return b.execShell(ctx, client, name, extract, io.Discard)
}

func (b *isloBackend) execShell(ctx context.Context, client isloAPI, name, command string, stdout io.Writer) error {
	code, err := client.ExecStream(ctx, name, &gosdk.ExecRequest{Command: []string{"bash", "-lc", command}}, stdout, b.rt.Stderr)
	if err != nil {
		return fmt.Errorf("islo exec %q: %w", command, err)
	}
	if code != 0 {
		return exit(code, "islo exec %q exited %d", command, code)
	}
	return nil
}

func createIsloSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, stderr io.Writer) (*os.File, error) {
	var input bytes.Buffer
	input.Write(manifest.NUL())
	archive, err := os.CreateTemp("", "crabbox-islo-sync-*.tgz")
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
