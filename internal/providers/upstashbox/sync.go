package upstashbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

func (b *backend) syncWorkspace(ctx context.Context, client api, boxID string, req RunRequest, workdir, folder string) ([]timingPhase, time.Duration, error) {
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
	if err := b.prepareWorkspace(ctx, client, boxID, folder, b.cfg.Sync.Delete); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStarted)
	archiveStarted := b.now()
	archive, err := createSyncArchive(ctx, req.Repo, manifest, b.rt.Stderr)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	archiveDuration := b.now().Sub(archiveStarted)
	uploadStarted := b.now()
	remoteArchive := workspacePath(".crabbox-upstash-box-sync-" + randomSuffix() + ".tgz")
	if err := client.UploadFile(ctx, boxID, archive.Name(), remoteArchive); err != nil {
		return nil, 0, fmt.Errorf("upstash-box upload archive: %w", err)
	}
	remoteArchiveName := path.Base(remoteArchive)
	extract := strings.Join([]string{
		"tar -xzf " + shellQuote(remoteArchiveName) + " -C " + shellQuote(folder),
		"rm -f " + shellQuote(remoteArchiveName),
	}, " && ")
	if err := b.execShell(ctx, client, boxID, extract, io.Discard); err != nil {
		if cleanupErr := cleanupRemoteFile(client, boxID, remoteArchiveName); cleanupErr != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: upstash-box sync cleanup failed for %s: %v\n", boxID, cleanupErr)
		}
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
		{Name: "upstash_box_sync", Ms: total.Milliseconds()},
	}, total, nil
}

func (b *backend) prepareWorkspace(ctx context.Context, client api, boxID, folder string, delete bool) error {
	folder = strings.Trim(strings.TrimSpace(folder), "/")
	if folder == "" || strings.Contains(folder, "..") {
		return exit(2, "upstash-box workspace folder %q is invalid", folder)
	}
	command := "mkdir -p " + shellQuote(folder)
	if delete {
		command = "rm -rf " + shellQuote(folder) + " && " + command
	}
	return b.execShell(ctx, client, boxID, command, io.Discard)
}

func (b *backend) execShell(ctx context.Context, client api, boxID, command string, stdout io.Writer) error {
	result, err := client.Exec(ctx, boxID, command, "")
	if err != nil {
		return fmt.Errorf("upstash-box exec %q: %w", command, err)
	}
	if stdout != nil && result.Output != "" {
		_, _ = io.WriteString(stdout, result.Output)
	}
	if result.ExitCode != 0 {
		return commandExitError("upstash-box exec "+command, result)
	}
	return nil
}

func createSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, _ io.Writer) (*os.File, error) {
	return createPortableSyncArchive(ctx, repo, manifest, "crabbox-upstash-box-sync-*.tgz")
}

func randomSuffix() string {
	return strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
}
