package replicate

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"time"
)

const replicateArchiveDataURLPrefix = "data:application/gzip;base64,"

type replicateArchiveInput struct {
	DataURL  string
	Phases   []timingPhase
	Duration time.Duration
	Size     int64
}

func buildReplicateArchiveDataURL(ctx context.Context, cfg Config, rt Runtime, repo Repo, force bool) (replicateArchiveInput, error) {
	now := time.Now
	if rt.Clock != nil {
		now = rt.Clock.Now
	}
	stderr := rt.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	start := now()
	excludes, err := syncExcludes(repo.Root, cfg)
	if err != nil {
		return replicateArchiveInput{}, err
	}
	manifestStarted := now()
	manifest, err := syncManifest(repo.Root, excludes, cfg.Sync.Includes)
	if err != nil {
		return replicateArchiveInput{}, exit(6, "build sync file list: %v", err)
	}
	manifestDuration := now().Sub(manifestStarted)
	preflightStarted := now()
	archiveManifest := manifest
	archiveManifest.Changed = nil
	archiveManifest.ChangedBytes = 0
	if err := checkSyncPreflight(archiveManifest, cfg, force, stderr); err != nil {
		return replicateArchiveInput{}, err
	}
	preflightDuration := now().Sub(preflightStarted)
	archiveStarted := now()
	archive, err := createReplicateSyncArchive(ctx, repo, manifest)
	if err != nil {
		return replicateArchiveInput{}, err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	archiveDuration := now().Sub(archiveStarted)
	info, err := archive.Stat()
	if err != nil {
		return replicateArchiveInput{}, exit(6, "stat replicate sync archive: %v", err)
	}
	size := info.Size()
	if cfg.Replicate.MaxArchiveBytes > 0 && size > cfg.Replicate.MaxArchiveBytes {
		return replicateArchiveInput{}, exit(6, "replicate archive too large: %d bytes > replicate.maxArchiveBytes %d; reduce sync scope or increase replicate.maxArchiveBytes intentionally", size, cfg.Replicate.MaxArchiveBytes)
	}
	encodeStarted := now()
	data, err := io.ReadAll(archive)
	if err != nil {
		return replicateArchiveInput{}, fmt.Errorf("read replicate sync archive: %w", err)
	}
	encodeDuration := now().Sub(encodeStarted)
	total := now().Sub(start)
	return replicateArchiveInput{
		DataURL: replicateArchiveDataURLPrefix + base64.StdEncoding.EncodeToString(data),
		Phases: []timingPhase{
			{Name: "manifest", Ms: manifestDuration.Milliseconds()},
			{Name: "preflight", Ms: preflightDuration.Milliseconds()},
			{Name: "archive", Ms: archiveDuration.Milliseconds()},
			{Name: "encode", Ms: encodeDuration.Milliseconds()},
			{Name: "replicate_archive_sync", Ms: total.Milliseconds()},
		},
		Duration: total,
		Size:     size,
	}, nil
}

func createReplicateSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest) (*os.File, error) {
	return coreCreateSyncArchive(ctx, repo, manifest, "crabbox-replicate-sync-*.tgz")
}
