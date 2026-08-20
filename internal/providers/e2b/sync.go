package e2b

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func (b *e2bBackend) syncWorkspace(ctx context.Context, client e2bAPI, session e2bSession, req RunRequest, workspace string, prepared ...*core.PreparedArchive) ([]timingPhase, time.Duration, error) {
	workspace, err := cleanE2BWorkspacePath(workspace)
	if err != nil {
		return nil, 0, err
	}
	return shared.RunSandboxArchiveSync(ctx, shared.SandboxArchiveSyncRequest{
		Config:              b.cfg,
		Repo:                req.Repo,
		ForceSyncLarge:      req.ForceSyncLarge,
		Workdir:             workspace,
		TempPattern:         "crabbox-e2b-sync-*.tgz",
		RemoteArchivePrefix: "crabbox-",
		PhaseName:           "e2b_sync",
		Provider:            e2bProvider,
		Stderr:              b.rt.Stderr,
		Now:                 b.now,
		Upload: func(uploadCtx context.Context, remoteArchive string, archive io.Reader) error {
			return e2bError("upload archive", client.UploadFile(uploadCtx, session, remoteArchive, archive))
		},
		Exec: func(execCtx context.Context, command string) error {
			return b.execShell(execCtx, client, session, command, io.Discard)
		},
	}, prepared...)
}

func (b *e2bBackend) prepareWorkspace(ctx context.Context, client e2bAPI, session e2bSession, workspace string) error {
	workspace, err := cleanE2BWorkspacePath(workspace)
	if err != nil {
		return err
	}
	return b.execShell(ctx, client, session, "mkdir -p "+shellQuote(workspace), io.Discard)
}

func cleanE2BWorkspacePath(workspace string) (string, error) {
	trimmed := strings.TrimSpace(workspace)
	if trimmed == "" {
		return "", exit(2, "e2b workspace path is empty")
	}
	clean := path.Clean(trimmed)
	if !strings.HasPrefix(clean, "/") {
		return "", exit(2, "e2b workspace path %q must resolve to an absolute path", workspace)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var":
		return "", exit(2, "e2b workspace path %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func (b *e2bBackend) execShell(ctx context.Context, client e2bAPI, session e2bSession, command string, stdout io.Writer) error {
	user, err := e2bProcessUser(b.cfg.E2B.User)
	if err != nil {
		return err
	}
	code, err := client.StartProcess(ctx, session, e2bProcessRequest{
		Command: command,
		User:    user,
		Timeout: b.cfg.TTL,
		Stdout:  stdout,
		Stderr:  b.rt.Stderr,
	})
	if err != nil {
		return fmt.Errorf("e2b exec %q: %w", command, err)
	}
	if code != 0 {
		return exit(code, "e2b exec %q exited %d", command, code)
	}
	return nil
}
