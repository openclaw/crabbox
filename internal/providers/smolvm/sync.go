package smolvm

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) syncWorkspace(ctx context.Context, client api, machineID string, req RunRequest, workdir, folder string) ([]timingPhase, time.Duration, error) {
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
	if err := b.prepareWorkspace(ctx, client, machineID, folder, b.cfg.Sync.Delete); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStarted)
	archiveStarted := b.now()
	archive, err := core.CreateSyncArchive(ctx, req.Repo, manifest, "crabbox-smolvm-sync-*.tgz")
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	archiveDuration := b.now().Sub(archiveStarted)
	uploadStarted := b.now()
	if err := client.InjectArchive(ctx, machineID, archive.Name(), folder); err != nil {
		return nil, 0, fmt.Errorf("smolvm inject archive: %w", err)
	}
	uploadDuration := b.now().Sub(uploadStarted)
	total := b.now().Sub(start)
	return []timingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "prepare", Ms: prepareDuration.Milliseconds()},
		{Name: "archive", Ms: archiveDuration.Milliseconds()},
		{Name: "inject", Ms: uploadDuration.Milliseconds()},
		{Name: "smolvm_sync", Ms: total.Milliseconds()},
	}, total, nil
}

func (b *backend) prepareWorkspace(ctx context.Context, client api, machineID, folder string, delete bool) error {
	f := strings.Trim(strings.TrimSpace(folder), "/")
	if f == "" || strings.Contains(f, "..") {
		// root case
		f = "workspace"
	}
	absFolder := "/" + strings.Trim(f, "/")
	if absFolder == "/" {
		absFolder = "/workspace"
	}
	command := "mkdir -p " + shellQuote(absFolder)
	if delete {
		if absFolder == "/workspace" {
			// safe clean for workdir root
			command = "find /workspace -mindepth 1 -exec rm -rf {} + 2>/dev/null || rm -rf -- /workspace/* 2>/dev/null || true; mkdir -p /workspace"
		} else {
			command = "rm -rf " + shellQuote(absFolder) + " && " + command
		}
	}
	return b.execShell(ctx, client, machineID, command, io.Discard)
}

func (b *backend) execShell(ctx context.Context, client api, machineID, command string, stdout io.Writer) error {
	result, err := client.Exec(ctx, machineID, command, "")
	if err != nil {
		return fmt.Errorf("smolvm exec %q: %w", command, err)
	}
	if stdout != nil && result.Output != "" {
		_, _ = io.WriteString(stdout, result.Output)
	}
	if result.ExitCode != 0 {
		return commandExitError("smolvm exec "+command, result)
	}
	return nil
}
