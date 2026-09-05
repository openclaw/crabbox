package smolvm

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) prepareArchive(ctx context.Context, req RunRequest) (*core.PreparedArchive, error) {
	return core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
		Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
		TempPattern: "crabbox-smolvm-sync-*.tgz", Stderr: b.rt.Stderr, Now: b.now,
	})
}

func (b *backend) syncWorkspace(ctx context.Context, client api, machineID string, req RunRequest, folder string, prepared *core.PreparedArchive) ([]timingPhase, time.Duration, error) {
	start := b.now()
	preparedExternally := prepared != nil
	if prepared == nil {
		var err error
		prepared, err = b.prepareArchive(ctx, req)
		if err != nil {
			return nil, 0, err
		}
	}
	defer prepared.Close()
	syncCtx := ctx
	if b.cfg.Sync.Timeout > 0 {
		remaining := max(time.Duration(0), b.cfg.Sync.Timeout-prepared.ArchiveDuration)
		var cancel context.CancelFunc
		syncCtx, cancel = context.WithTimeout(ctx, remaining)
		defer cancel()
	}
	prepareStarted := b.now()
	if err := b.prepareWorkspace(syncCtx, client, machineID, folder, b.cfg.Sync.Delete); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStarted)
	uploadStarted := b.now()
	if err := client.InjectArchive(syncCtx, machineID, prepared.File.Name(), folder); err != nil {
		return nil, 0, fmt.Errorf("smolvm inject archive: %w", err)
	}
	uploadDuration := b.now().Sub(uploadStarted)
	total := b.now().Sub(start)
	if preparedExternally {
		total += prepared.ManifestDuration + prepared.PreflightDuration + prepared.ArchiveDuration
	}
	return []timingPhase{
		{Name: "manifest", Ms: prepared.ManifestDuration.Milliseconds()},
		{Name: "preflight", Ms: prepared.PreflightDuration.Milliseconds()},
		{Name: "archive", Ms: prepared.ArchiveDuration.Milliseconds()},
		{Name: "prepare", Ms: prepareDuration.Milliseconds()},
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
