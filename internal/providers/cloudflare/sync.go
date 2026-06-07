package cloudflare

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

func (b *cloudflareBackend) syncWorkspace(ctx context.Context, client *cloudflareClient, sandboxID string, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
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
	if err := b.prepareWorkspace(syncCtx, client, sandboxID, workdir, b.cfg.Sync.Delete); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStarted)
	archiveStarted := b.now()
	archive, err := createCloudflareSyncArchive(syncCtx, req.Repo, manifest, b.rt.Stderr)
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	archiveInfo, err := archive.Stat()
	if err != nil {
		return nil, 0, fmt.Errorf("stat sync archive: %w", err)
	}
	archiveDuration := b.now().Sub(archiveStarted)
	diskStarted := b.now()
	if err := b.checkRemoteDiskForSync(syncCtx, client, sandboxID, workdir, manifest.Bytes, archiveInfo.Size()); err != nil {
		return nil, 0, err
	}
	diskDuration := b.now().Sub(diskStarted)
	uploadStarted := b.now()
	remoteArchive := remoteArchivePath()
	if err := client.uploadFile(syncCtx, sandboxID, archive.Name(), remoteArchive); err != nil {
		return nil, 0, fmt.Errorf("upload archive: %w", err)
	}
	if err := b.execShell(syncCtx, client, sandboxID, cloudflareExtractArchiveCommand(remoteArchive, workdir), io.Discard); err != nil {
		return nil, 0, err
	}
	uploadDuration := b.now().Sub(uploadStarted)
	total := b.now().Sub(start)
	return []timingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "prepare", Ms: prepareDuration.Milliseconds()},
		{Name: "archive", Ms: archiveDuration.Milliseconds()},
		{Name: "disk", Ms: diskDuration.Milliseconds()},
		{Name: "upload", Ms: uploadDuration.Milliseconds()},
		{Name: "cloudflare_sync", Ms: total.Milliseconds()},
	}, total, nil
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

func (b *cloudflareBackend) prepareWorkspace(ctx context.Context, client *cloudflareClient, sandboxID, workdir string, deleteContents bool) error {
	command := "mkdir -p " + shellQuote(workdir)
	if deleteContents {
		command = "rm -rf " + shellQuote(workdir) + " && " + command
	}
	return b.execShell(ctx, client, sandboxID, command, io.Discard)
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

func cloudflareExtractArchiveCommand(remoteArchive, workdir string) string {
	return strings.Join([]string{
		"tar -xzf " + shellQuote(remoteArchive) + " -C " + shellQuote(workdir),
		"status=$?",
		"rm -f " + shellQuote(remoteArchive),
		"cleanup=$?",
		`if [ "$status" -ne 0 ]; then exit "$status"; fi`,
		`exit "$cleanup"`,
	}, "; ")
}

func createCloudflareSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, _ io.Writer) (*os.File, error) {
	return createPortableSyncArchive(ctx, repo, manifest, "crabbox-cloudflare-sync-*.tgz")
}
