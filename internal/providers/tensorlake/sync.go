package tensorlake

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

// rejectIncompatibleSyncOptions refuses Crabbox sync flags whose semantics
// can't be honored on top of `tensorlake sbx cp`. SyncOnly and ChecksumSync
// require Crabbox-side rsync semantics that the Tensorlake CLI doesn't
// expose. ForceSyncLarge is rejected by the core delegated-sync gate before
// we get here, so we don't repeat that check.
func rejectIncompatibleSyncOptions(req RunRequest) error {
	if req.SyncOnly {
		return exit(2, "provider=tensorlake uses archive sync; --sync-only is not supported")
	}
	if req.ChecksumSync {
		return exit(2, "provider=tensorlake uses archive sync; --checksum is not supported")
	}
	return nil
}

// syncWorkspace builds a gzipped tar archive of the repo, uploads it via
// `tensorlake sbx cp`, and extracts it into the configured workdir. Returns
// per-phase timings so warmup/run summaries can break the sync down the same
// way islo and daytona do.
func (b *tensorlakeBackend) syncWorkspace(ctx context.Context, cli *tensorlakeCLI, sandboxID string, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
	start := b.now()

	excludes, err := syncExcludes(req.Repo.Root, b.cfg)
	if err != nil {
		return nil, 0, err
	}

	manifestStart := b.now()
	manifest, err := syncManifest(req.Repo.Root, excludes)
	if err != nil {
		return nil, 0, exit(6, "build sync file list: %v", err)
	}
	manifestDuration := b.now().Sub(manifestStart)

	preflightStart := b.now()
	if err := checkSyncPreflight(manifest, b.cfg, req.ForceSyncLarge, b.rt.Stderr); err != nil {
		return nil, 0, err
	}
	preflightDuration := b.now().Sub(preflightStart)

	prepareStart := b.now()
	if err := b.prepareWorkspace(ctx, cli, sandboxID, workdir); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStart)

	archiveStart := b.now()
	archive, err := createTensorlakeSyncArchive(ctx, req.Repo, manifest, b.rt.Stderr)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	archiveDuration := b.now().Sub(archiveStart)

	uploadStart := b.now()
	remoteArchive := path.Join("/tmp", "crabbox-sync-"+randomSuffix()+".tgz")
	if err := cli.uploadFile(ctx, sandboxID, archive.Name(), remoteArchive); err != nil {
		return nil, 0, err
	}
	uploadDuration := b.now().Sub(uploadStart)

	extractStart := b.now()
	if err := b.extractRemoteArchive(ctx, cli, sandboxID, remoteArchive, workdir); err != nil {
		// Best-effort cleanup of the remote archive even when extract fails.
		_ = cli.execShell(context.Background(), sandboxID, "rm -f "+shellQuote(remoteArchive))
		return nil, 0, err
	}
	extractDuration := b.now().Sub(extractStart)

	total := b.now().Sub(start)
	return []timingPhase{
		{Name: "manifest", Ms: manifestDuration.Milliseconds()},
		{Name: "preflight", Ms: preflightDuration.Milliseconds()},
		{Name: "prepare", Ms: prepareDuration.Milliseconds()},
		{Name: "archive", Ms: archiveDuration.Milliseconds()},
		{Name: "upload", Ms: uploadDuration.Milliseconds()},
		{Name: "extract", Ms: extractDuration.Milliseconds()},
		{Name: "tensorlake_sync", Ms: total.Milliseconds()},
	}, total, nil
}

func (b *tensorlakeBackend) prepareWorkspace(ctx context.Context, cli *tensorlakeCLI, sandboxID, workdir string) error {
	command := "mkdir -p " + shellQuote(workdir)
	if b.cfg.Sync.Delete {
		command = "rm -rf " + shellQuote(workdir) + " && " + command
	}
	return cli.execShell(ctx, sandboxID, command)
}

func (b *tensorlakeBackend) extractRemoteArchive(ctx context.Context, cli *tensorlakeCLI, sandboxID, remoteArchive, workdir string) error {
	cmd := strings.Join([]string{
		"tar -xzf " + shellQuote(remoteArchive) + " -C " + shellQuote(workdir),
		"rm -f " + shellQuote(remoteArchive),
	}, " && ")
	return cli.execShell(ctx, sandboxID, cmd)
}

func createTensorlakeSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, stderr io.Writer) (*os.File, error) {
	var input bytes.Buffer
	input.Write(manifest.NUL())
	archive, err := os.CreateTemp("", "crabbox-tensorlake-sync-*.tgz")
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
