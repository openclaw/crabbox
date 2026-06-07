package modal

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

func (b *modalBackend) syncWorkspace(ctx context.Context, client modalAPI, sandboxID string, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
	workdir, err := cleanModalWorkdir(workdir)
	if err != nil {
		return nil, 0, err
	}
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
	if err := b.prepareWorkspace(ctx, client, sandboxID, workdir, b.cfg.Sync.Delete); err != nil {
		return nil, 0, err
	}
	prepareDuration := b.now().Sub(prepareStarted)
	archiveStarted := b.now()
	archive, err := createModalSyncArchive(ctx, req.Repo, manifest, b.rt.Stderr)
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	archiveDuration := b.now().Sub(archiveStarted)
	uploadStarted := b.now()
	remoteArchive := path.Join("/tmp", "crabbox-modal-sync-"+modalRandomSuffix()+".tgz")
	if err := client.UploadFile(ctx, sandboxID, archive.Name(), remoteArchive); err != nil {
		return nil, 0, modalError("upload archive", err)
	}
	extract := strings.Join([]string{
		"tar -xzf " + shellQuote(remoteArchive) + " -C " + shellQuote(workdir),
		"rm -f " + shellQuote(remoteArchive),
	}, " && ")
	if err := b.execShell(ctx, client, sandboxID, extract, io.Discard); err != nil {
		_ = b.execShell(context.Background(), client, sandboxID, "rm -f "+shellQuote(remoteArchive), io.Discard)
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
		{Name: "modal_sync", Ms: total.Milliseconds()},
	}, total, nil
}

func (b *modalBackend) prepareWorkspace(ctx context.Context, client modalAPI, sandboxID, workdir string, delete bool) error {
	workdir, err := cleanModalWorkdir(workdir)
	if err != nil {
		return err
	}
	command := "mkdir -p " + shellQuote(workdir)
	if delete {
		command = "rm -rf " + shellQuote(workdir) + " && " + command
	}
	return b.execShell(ctx, client, sandboxID, command, io.Discard)
}

func (b *modalBackend) execShell(ctx context.Context, client modalAPI, sandboxID, command string, stdout io.Writer) error {
	code, err := client.Exec(ctx, modalExecRequest{
		SandboxID: sandboxID,
		Command:   []string{"bash", "-lc", command},
		Timeout:   durationSecondsCeil(modalTimeoutDuration(b.cfg.TTL)),
		Stdout:    stdout,
		Stderr:    b.rt.Stderr,
	})
	if err != nil {
		return fmt.Errorf("modal exec %q: %w", command, err)
	}
	if code != 0 {
		return exit(code, "modal exec %q exited %d", command, code)
	}
	return nil
}

func createModalSyncArchive(ctx context.Context, repo Repo, manifest SyncManifest, _ io.Writer) (*os.File, error) {
	return createPortableSyncArchive(ctx, repo, manifest, "crabbox-modal-sync-*.tgz")
}

func modalRandomSuffix() string {
	return strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
}
