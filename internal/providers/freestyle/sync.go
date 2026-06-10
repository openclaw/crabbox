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

const maxFreestyleArchiveUploadBytes = 64 << 20

func (b *freestyleBackend) syncWorkspace(ctx context.Context, client freestyleAPI, name string, req RunRequest) ([]timingPhase, time.Duration, error) {
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
	workspace, err := freestyleWorkspacePath(b.cfg)
	if err != nil {
		return nil, 0, err
	}
	prepareStarted := b.now()
	if err := b.prepareWorkspace(ctx, client, name, workspace, b.cfg.Sync.Delete); err != nil {
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
	archiveData, err := readFreestyleArchiveForUpload(archive)
	if err != nil {
		return nil, 0, fmt.Errorf("freestyle read archive: %w", err)
	}
	b64Content := base64.StdEncoding.EncodeToString(archiveData)
	suffix := freestyleRandomSuffix()
	remoteArchive := "/tmp/crabbox-" + suffix + ".tgz"
	if err := client.WriteFile(ctx, name, remoteArchive, b64Content, "base64"); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: freestyle file API upload failed; falling back to exec upload: %v\n", err)
		if fallbackErr := b.uploadArchiveViaExec(ctx, client, name, workspace, archiveData); fallbackErr != nil {
			return nil, 0, fallbackErr
		}
	} else {
		if err := b.execShell(ctx, client, name, freestyleDirectExtractCommand(remoteArchive, workspace)); err != nil {
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
	if err := b.execShell(ctx, client, name, "rm -f "+shellQuote(remoteB64)+" "+shellQuote(remoteArchive)); err != nil {
		return err
	}
	buf := archiveData
	chunkSize := 48 * 1024
	for i := 0; i < len(buf); i += chunkSize {
		end := i + chunkSize
		if end > len(buf) {
			end = len(buf)
		}
		chunk := base64.StdEncoding.EncodeToString(buf[i:end])
		command := "printf %s " + shellQuote(chunk) + " >> " + shellQuote(remoteB64)
		if err := b.execShell(ctx, client, name, command); err != nil {
			return err
		}
	}
	return b.execShell(ctx, client, name, freestyleFallbackExtractCommand(remoteB64, remoteArchive, workspace))
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
