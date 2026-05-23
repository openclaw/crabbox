package freestyle

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

	core "github.com/openclaw/crabbox/internal/cli"
)

type SyncManifest = core.SyncManifest

func rejectFreestyleSyncOptions(req RunRequest) error {
	if req.SyncOnly {
		return exit(2, "%s uses Freestyle archive sync; --sync-only is not supported", freestyleProvider)
	}
	if req.ChecksumSync {
		return exit(2, "%s uses Freestyle archive sync; --checksum is not supported", freestyleProvider)
	}
	return nil
}

func (b *freestyleBackend) syncWorkspace(ctx context.Context, client freestyleAPI, name string, req RunRequest) ([]timingPhase, time.Duration, error) {
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
	workspace, err := freestyleWorkspacePath(b.cfg)
	if err != nil {
		return nil, 0, err
	}
	prepareStarted := b.now()
	if err := b.prepareWorkspace(ctx, client, name, workspace); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStarted)
	archiveStarted := b.now()
	archive, err := createFreestyleSyncArchive(ctx, req.Repo, manifest, b.rt.Stderr)
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	archiveDuration := b.now().Sub(archiveStarted)
	uploadStarted := b.now()
	if _, err := archive.Seek(0, 0); err != nil {
		return nil, 0, fmt.Errorf("freestyle rewind archive: %w", err)
	}
	archiveData, err := io.ReadAll(archive)
	if err != nil {
		return nil, 0, fmt.Errorf("freestyle read archive: %w", err)
	}
	suffix := freestyleRandomSuffix()
	remoteArchive := path.Join("/tmp", "crabbox-"+suffix+".tgz")
	if err := client.WriteFile(ctx, name, remoteArchive, archiveData); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: freestyle WriteFile upload failed; falling back to exec upload: %v\n", err)
		if fallbackErr := b.uploadArchiveViaExec(ctx, client, name, workspace, archiveData); fallbackErr != nil {
			return nil, 0, fallbackErr
		}
	} else {
		if err := b.execShell(ctx, client, name, "tar -xzf "+shellQuote(remoteArchive)+" -C "+shellQuote(workspace)+"; R=$?; rm -f "+shellQuote(remoteArchive)+"; exit $R", io.Discard); err != nil {
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
		{Name: "freestyle_sync", Ms: total.Milliseconds()},
	}, total, nil
}

func (b *freestyleBackend) prepareWorkspace(ctx context.Context, client freestyleAPI, name, workspace string) error {
	command := "mkdir -p " + shellQuote(workspace)
	if b.cfg.Sync.Delete {
		command = "rm -rf " + shellQuote(workspace) + " && " + command
	}
	return b.execShell(ctx, client, name, command, io.Discard)
}

func (b *freestyleBackend) uploadArchiveViaExec(ctx context.Context, client freestyleAPI, name, workspace string, archiveData []byte) error {
	suffix := freestyleRandomSuffix()
	remoteB64 := path.Join("/tmp", "crabbox-"+suffix+".tgz.b64")
	remoteArchive := path.Join("/tmp", "crabbox-"+suffix+".tgz")
	if err := b.execShell(ctx, client, name, "rm -f "+shellQuote(remoteB64)+" "+shellQuote(remoteArchive), io.Discard); err != nil {
		return err
	}
	chunkSize := 48 * 1024
	for i := 0; i < len(archiveData); i += chunkSize {
		end := i + chunkSize
		if end > len(archiveData) {
			end = len(archiveData)
		}
		chunk := base64.StdEncoding.EncodeToString(archiveData[i:end])
		command := "printf %s " + shellQuote(chunk) + " >> " + shellQuote(remoteB64)
		if err := b.execShell(ctx, client, name, command, io.Discard); err != nil {
			return err
		}
	}
	return b.execShell(ctx, client, name, freestyleFallbackExtractCommand(remoteB64, remoteArchive, workspace), io.Discard)
}

func freestyleFallbackExtractCommand(remoteB64, remoteArchive, workspace string) string {
	extract := strings.Join([]string{
		"if base64 -d " + shellQuote(remoteB64) + " > " + shellQuote(remoteArchive) + " 2>/dev/null; then :; else base64 --decode " + shellQuote(remoteB64) + " > " + shellQuote(remoteArchive) + "; fi",
		"tar -xzf " + shellQuote(remoteArchive) + " -C " + shellQuote(workspace),
	}, " && ")
	cleanup := "rm -f " + shellQuote(remoteB64) + " " + shellQuote(remoteArchive)
	return extract + "; status=$?; " + cleanup + "; exit $status"
}

func (b *freestyleBackend) execShell(ctx context.Context, client freestyleAPI, name, command string, stdout io.Writer) error {
	code, err := client.Exec(ctx, name, "bash -lc "+shellQuote(command), stdout, b.rt.Stderr)
	if err != nil {
		return fmt.Errorf("freestyle exec %q: %w", command, err)
	}
	if code != 0 {
		return exit(code, "freestyle exec %q exited %d", command, code)
	}
	return nil
}

func createFreestyleSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, stderr io.Writer) (*os.File, error) {
	var input bytes.Buffer
	input.Write(manifest.NUL())
	archive, err := os.CreateTemp("", "crabbox-freestyle-sync-*.tgz")
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
