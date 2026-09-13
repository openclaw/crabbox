package shared

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

// EnvdWorkspace owns workspace preparation over the compatible envd protocol.
// Adapters supply their user default and archive naming; the client retains
// authentication, endpoint routing, and process-completion interpretation.
type EnvdWorkspace struct {
	Provider            string
	Config              core.Config
	Runtime             core.Runtime
	Workdir             string
	User                string
	DefaultUser         string
	RemoteArchivePrefix string
}

func (w EnvdWorkspace) ProcessUser() (string, error) {
	clean := strings.TrimSpace(w.User)
	if clean == "" {
		return w.DefaultUser, nil
	}
	if clean == "." || clean == ".." || strings.ContainsAny(clean, `/\`) || strings.ContainsRune(clean, 0) {
		return "", core.Exit(2, "invalid %s.user %q: use a login name, not a path", w.Provider, w.User)
	}
	return clean, nil
}

func (w EnvdWorkspace) Path() string {
	workdir := core.Blank(strings.TrimSpace(w.Workdir), "crabbox")
	if strings.HasPrefix(workdir, "/") {
		return path.Clean(workdir)
	}
	user, err := w.ProcessUser()
	if err != nil || user == "" {
		user = core.Blank(w.DefaultUser, "user")
	}
	home := path.Join("/home", user)
	if user == "root" {
		home = "/root"
	}
	return path.Join(home, workdir)
}

func (w EnvdWorkspace) Sync(ctx context.Context, client EnvdSandboxAPI, session EnvdSandboxSession, req core.RunRequest, workspace string, prepared ...*core.PreparedArchive) ([]core.TimingPhase, time.Duration, error) {
	workspace, err := CleanPOSIXWorkspacePath(w.Provider+" workspace path", workspace)
	if err != nil {
		return nil, 0, err
	}
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config: w.Config, Repo: req.Repo, ForceSyncLarge: req.ForceSyncLarge,
		Workdir: workspace, TempPattern: "crabbox-" + w.Provider + "-sync-*.tgz",
		RemoteArchivePrefix: w.RemoteArchivePrefix, PhaseName: w.Provider + "_sync", Provider: w.Provider,
		Stderr: w.Runtime.Stderr, Now: func() time.Time { return core.ClockNow(w.Runtime.Clock) },
		Upload: func(ctx context.Context, archivePath string, archive io.Reader) error {
			if err := client.UploadFile(ctx, session, archivePath, archive); err != nil {
				return fmt.Errorf("%s upload archive: %w", w.Provider, err)
			}
			return nil
		},
		Exec: func(ctx context.Context, command string) error {
			return w.exec(ctx, client, session, command)
		},
	}, prepared...)
}

func (w EnvdWorkspace) Prepare(ctx context.Context, client EnvdSandboxAPI, session EnvdSandboxSession, workspace string) error {
	workspace, err := CleanPOSIXWorkspacePath(w.Provider+" workspace path", workspace)
	if err != nil {
		return err
	}
	return w.exec(ctx, client, session, "mkdir -p "+core.ShellQuote(workspace))
}

func (w EnvdWorkspace) exec(ctx context.Context, client EnvdSandboxAPI, session EnvdSandboxSession, command string) error {
	user, err := w.ProcessUser()
	if err != nil {
		return err
	}
	code, err := client.StartProcess(ctx, session, EnvdSandboxProcessRequest{
		Command: command, User: user, Timeout: w.Config.TTL,
		Stdout: io.Discard, Stderr: w.Runtime.Stderr,
	})
	if err != nil {
		return fmt.Errorf("%s exec %q: %w", w.Provider, command, err)
	}
	if code != 0 {
		return core.Exit(code, "%s exec %q exited %d", w.Provider, command, code)
	}
	return nil
}
