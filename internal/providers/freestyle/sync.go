package freestyle

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const maxFreestyleArchiveUploadBytes = 64 << 20

func (b *freestyleBackend) prepareArchive(ctx context.Context, req RunRequest) (*core.PreparedArchive, error) {
	archive, err := core.PrepareDelegatedArchive(ctx, core.DelegatedArchivePreparationRequest{
		Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
		TempPattern: "crabbox-freestyle-sync-*.tgz", Stderr: b.rt.Stderr, Now: b.now,
	})
	if err != nil {
		return nil, err
	}
	if err := checkFreestyleArchiveSize(archive.Size); err != nil {
		_ = archive.Close()
		return nil, err
	}
	return archive, nil
}

func (b *freestyleBackend) syncWorkspace(ctx context.Context, client freestyleAPI, name string, req RunRequest, prepared *core.PreparedArchive) ([]timingPhase, time.Duration, error) {
	workspace, err := freestyleWorkspacePath(b.cfg)
	if err != nil {
		return nil, 0, err
	}
	if prepared == nil {
		prepared, err = b.prepareArchive(ctx, req)
		if err != nil {
			return nil, 0, err
		}
	}
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config: b.cfg, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
		Workdir: workspace, TempPattern: "crabbox-freestyle-sync-*.tgz",
		RemoteArchiveDir: "/tmp", RemoteArchivePrefix: "crabbox-freestyle-sync-",
		Provider: freestyleProvider, PhaseName: "freestyle_sync", Stderr: b.rt.Stderr, Now: b.now,
		CleanupContext: freestyleSyncCleanupContext,
		Upload: func(ctx context.Context, remotePath string, body io.Reader) error {
			return b.uploadArchive(ctx, client, name, remotePath, body)
		},
		Exec: func(ctx context.Context, command string) error {
			return b.execShell(ctx, client, name, command)
		},
	}, prepared)
}

func (b *freestyleBackend) prepareWorkspace(ctx context.Context, client freestyleAPI, name, workspace string) error {
	return b.execShell(ctx, client, name, "mkdir -p "+shellQuote(workspace))
}

func (b *freestyleBackend) uploadArchive(ctx context.Context, client freestyleAPI, name, remoteArchive string, body io.Reader) error {
	data, err := readFreestyleArchiveForUpload(body)
	if err != nil {
		return fmt.Errorf("freestyle read archive: %w", err)
	}
	// An ambiguous late file-API write must not overwrite the fallback archive.
	apiPath, remoteB64, decoded := remoteArchive+".api", remoteArchive+".exec.b64", remoteArchive+".exec"
	defer b.cleanupFreestyleUpload(ctx, client, name, apiPath, remoteB64, decoded)
	if err := client.WriteFile(ctx, name, apiPath, base64.StdEncoding.EncodeToString(data), "base64"); err == nil {
		return b.execShell(ctx, client, name, "mv -f "+shellQuote(apiPath)+" "+shellQuote(remoteArchive))
	} else {
		fmt.Fprintf(b.rt.Stderr, "warning: freestyle file API upload failed; falling back to exec upload: %v\n", err)
	}
	if err := b.execShell(ctx, client, name, "rm -f "+shellQuote(remoteB64)+" "+shellQuote(decoded)); err != nil {
		return err
	}
	const chunkSize = 48 * 1024
	chunkCount := (len(data) + chunkSize - 1) / chunkSize
	for i := 0; i < len(data); i += chunkSize {
		end := min(i+chunkSize, len(data))
		chunk := base64.StdEncoding.EncodeToString(data[i:end])
		command := "printf %s " + shellQuote(chunk) + " >> " + shellQuote(remoteB64)
		action := fmt.Sprintf("upload archive chunk %d/%d", i/chunkSize+1, chunkCount)
		if err := b.execShellRedacted(ctx, client, name, command, action); err != nil {
			return err
		}
	}
	decode := "if base64 -d < " + shellQuote(remoteB64) + " > " + shellQuote(decoded) + " 2>/dev/null; then :; else base64 --decode < " + shellQuote(remoteB64) + " > " + shellQuote(decoded) + "; fi"
	return b.execShell(ctx, client, name, decode+" && mv -f "+shellQuote(decoded)+" "+shellQuote(remoteArchive))
}

func freestyleSyncCleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), freestyleCleanupTimeout)
}

func (b *freestyleBackend) cleanupFreestyleUpload(parent context.Context, client freestyleAPI, name string, paths ...string) {
	quoted := make([]string, 0, len(paths))
	for _, remotePath := range paths {
		quoted = append(quoted, shellQuote(remotePath))
	}
	cleanupCtx, cancel := freestyleSyncCleanupContext(parent)
	defer cancel()
	if err := b.execShell(cleanupCtx, client, name, "rm -f "+strings.Join(quoted, " ")); err != nil && b.rt.Stderr != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: freestyle upload cleanup failed for %s: %v\n", name, err)
	}
}

func checkFreestyleArchiveSize(size int64) error {
	if size > maxFreestyleArchiveUploadBytes {
		return exit(6, "freestyle sync archive exceeds %d bytes after compression; narrow sync.include/excludes or split the run", maxFreestyleArchiveUploadBytes)
	}
	return nil
}

func readFreestyleArchiveForUpload(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, maxFreestyleArchiveUploadBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if err := checkFreestyleArchiveSize(int64(len(data))); err != nil {
		return nil, err
	}
	return data, nil
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

func (b *freestyleBackend) execShellRedacted(ctx context.Context, client freestyleAPI, name, command, action string) error {
	code, err := client.Exec(ctx, name, "bash -lc "+shellQuote(command), io.Discard, b.rt.Stderr)
	if err != nil {
		return fmt.Errorf("freestyle %s: %w", action, err)
	}
	if code != 0 {
		return exit(code, "freestyle %s exited %d", action, code)
	}
	return nil
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
