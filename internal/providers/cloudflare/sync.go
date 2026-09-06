package cloudflare

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *cloudflareBackend) prepareArchive(ctx context.Context, req RunRequest) (*core.PreparedArchive, error) {
	return core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
		Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
		TempPattern: "crabbox-cloudflare-sync-*.tgz", Stderr: b.rt.Stderr, Now: b.now,
	})
}

func (b *cloudflareBackend) syncWorkspace(ctx context.Context, client *cloudflareClient, sandboxID string, req RunRequest, workdir string, prepared *core.PreparedArchive) ([]timingPhase, time.Duration, error) {
	if prepared == nil {
		var err error
		prepared, err = b.prepareArchive(ctx, req)
		if err != nil {
			return nil, 0, err
		}
	}
	var diskDuration time.Duration
	phases, total, err := core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge, Workdir: workdir,
		Provider: providerName, PhaseName: "cloudflare_sync", RemoteArchivePrefix: "crabbox-cloudflare-sync-",
		Stderr: b.rt.Stderr, Now: b.now,
		CleanupContext: func(context.Context) (context.Context, context.CancelFunc) { return cloudflareCleanupContext() },
		Upload: func(uploadCtx context.Context, remoteArchive string, _ io.Reader) error {
			start := b.now()
			if err := b.prepareWorkspace(uploadCtx, client, sandboxID, workdir); err != nil {
				return err
			}
			if err := b.checkRemoteDiskForSync(uploadCtx, client, sandboxID, workdir, prepared.Manifest.Bytes, prepared.Size); err != nil {
				return err
			}
			diskDuration = b.now().Sub(start)
			// Reopen the same owned snapshot to preserve this transport's exact Content-Length.
			if err := client.uploadFile(uploadCtx, sandboxID, prepared.File.Name(), remoteArchive); err != nil {
				return fmt.Errorf("upload archive: %w", err)
			}
			return nil
		},
		Exec: func(execCtx context.Context, command string) error {
			return b.execShell(execCtx, client, sandboxID, command, io.Discard)
		},
	}, prepared)
	for i := range phases {
		if phases[i].Name == "upload" {
			phases[i].Ms -= diskDuration.Milliseconds()
			phases = slices.Insert(phases, i, timingPhase{Name: "disk", Ms: diskDuration.Milliseconds()})
			break
		}
	}
	return phases, total, err
}

func (b *cloudflareBackend) checkRemoteDiskForSync(ctx context.Context, client *cloudflareClient, sandboxID, workdir string, manifestBytes, archiveBytes int64) error {
	required := manifestBytes + archiveBytes
	if required <= 0 {
		return nil
	}
	available, ok, err := b.remoteDiskAvailable(ctx, client, sandboxID, workdir)
	if err != nil {
		return err
	}
	if !ok {
		return exit(6, "%s could not determine remote disk headroom for sync", providerName)
	}
	if available <= 0 {
		return exit(6, "%s remote disk too small for sync: need %s for archive+extract, available %s; use a larger Cloudflare instance_type or reduce sync.exclude", providerName, byteCount(required), byteCount(available))
	}
	if available < required {
		return exit(6, "%s remote disk too small for sync: need %s for archive+extract, available %s; use a larger Cloudflare instance_type or reduce sync.exclude", providerName, byteCount(required), byteCount(available))
	}
	const lowHeadroom = 1 << 30
	if remaining := available - required; remaining < lowHeadroom {
		fmt.Fprintf(b.rt.Stderr, "warning: %s remote disk headroom after sync is low: %s\n", providerName, byteCount(remaining))
	}
	return nil
}

func (b *cloudflareBackend) remoteDiskAvailable(ctx context.Context, client *cloudflareClient, sandboxID, workdir string) (int64, bool, error) {
	command := "set -o pipefail; df -B1 --output=avail,target /tmp " + shellQuote(workdir) + " | tail -n +2"
	var stdout bytes.Buffer
	if err := b.execShell(ctx, client, sandboxID, command, &stdout); err != nil {
		return 0, false, err
	}
	var minAvailable int64
	found := false
	for _, line := range strings.Split(stdout.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		available, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || available < 0 {
			continue
		}
		if !found || available < minAvailable {
			minAvailable = available
			found = true
		}
	}
	return minAvailable, found, nil
}

func byteCount(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}

func (b *cloudflareBackend) prepareWorkspace(ctx context.Context, client *cloudflareClient, sandboxID, workdir string) error {
	return b.execShell(ctx, client, sandboxID, "mkdir -p "+shellQuote(workdir), io.Discard)
}

func (b *cloudflareBackend) execShell(ctx context.Context, client *cloudflareClient, sandboxID, command string, stdout io.Writer) error {
	code, err := client.execStream(ctx, sandboxID, execStreamRequest{
		Command:   command,
		Cwd:       "/",
		TimeoutMS: durationMillisecondsCeil(b.cfg.TTL),
	}, stdout, b.rt.Stderr)
	if err != nil {
		return fmt.Errorf("%s exec %q: %w", providerName, command, err)
	}
	if code != 0 {
		return exit(code, "%s exec %q exited %d", providerName, command, code)
	}
	return nil
}
