package cubesandbox

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *cubesandboxBackend) syncWorkspace(ctx context.Context, client cubesandboxAPI, session cubesandboxSession, req core.RunRequest, workspace string, prepared ...*core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
	workspace, err := cleanCubeSandboxWorkspacePath(workspace)
	if err != nil {
		return nil, 0, err
	}
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config:              b.cfg,
		Repo:                req.Repo,
		ForceSyncLarge:      req.ForceSyncLarge,
		Workdir:             workspace,
		TempPattern:         "crabbox-cubesandbox-sync-*.tgz",
		RemoteArchiveDir:    "/tmp",
		RemoteArchivePrefix: "crabbox-cubesandbox-sync-",
		PhaseName:           "cubesandbox_sync",
		Provider:            providerName,
		Stderr:              b.rt.Stderr,
		Now:                 func() time.Time { return core.ClockNow(b.rt.Clock) },
		Upload: func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
			if err := client.UploadFile(uploadCtx, session, remoteArchive, body); err != nil {
				return cubesandboxError("upload archive", err)
			}
			return nil
		},
		Exec: func(execCtx context.Context, command string) error {
			return b.execShell(execCtx, client, session, command, io.Discard)
		},
	}, prepared...)
}

func (b *cubesandboxBackend) prepareWorkspace(ctx context.Context, client cubesandboxAPI, session cubesandboxSession, workspace string) error {
	workspace, err := cleanCubeSandboxWorkspacePath(workspace)
	if err != nil {
		return err
	}
	return b.execShell(ctx, client, session, "mkdir -p "+core.ShellQuote(workspace), io.Discard)
}

func cleanCubeSandboxWorkspacePath(workspace string) (string, error) {
	trimmed := strings.TrimSpace(workspace)
	if trimmed == "" {
		return "", core.Exit(2, "cubesandbox workspace path is empty")
	}
	clean := path.Clean(trimmed)
	if !strings.HasPrefix(clean, "/") {
		return "", core.Exit(2, "cubesandbox workspace path %q must resolve to an absolute path", workspace)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var":
		return "", core.Exit(2, "cubesandbox workspace path %q is too broad; choose a dedicated subdirectory", clean)
	}
	return clean, nil
}

func (b *cubesandboxBackend) execShell(ctx context.Context, client cubesandboxAPI, session cubesandboxSession, command string, stdout io.Writer) error {
	user, err := cubesandboxProcessUser(b.cfg.CubeSandbox.User)
	if err != nil {
		return err
	}
	code, err := client.StartProcess(ctx, session, cubesandboxProcessRequest{
		Command: command,
		User:    user,
		Timeout: b.cfg.TTL,
		Stdout:  stdout,
		Stderr:  b.rt.Stderr,
	})
	if err != nil {
		return fmt.Errorf("cubesandbox exec %q: %w", command, err)
	}
	if code != 0 {
		return core.Exit(code, "cubesandbox exec %q exited %d", command, code)
	}
	return nil
}
