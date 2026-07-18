package cloudrunsandbox

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (b *backend) syncWorkspace(ctx context.Context, transport sandboxTransport, sandboxID string, req RunRequest, workdir string) ([]timingPhase, time.Duration, error) {
	return core.RunDelegatedArchiveSync(ctx, core.DelegatedArchiveSyncRequest{
		Config:              b.cfg,
		Repo:                req.Repo,
		ForceSyncLarge:      req.ForceSyncLarge,
		Workdir:             workdir,
		TempPattern:         "crabbox-cloud-run-sandbox-sync-*.tgz",
		RemoteArchiveDir:    "/tmp",
		RemoteArchivePrefix: "crabbox-sync-",
		PhaseName:           "cloud_run_sandbox_sync",
		Provider:            providerName,
		Stderr:              b.rt.Stderr,
		Now:                 b.now,
		CleanupContext:      b.cleanupContext,
		Upload: func(uploadCtx context.Context, remoteArchive string, body io.Reader) error {
			return b.uploadArchive(uploadCtx, transport, sandboxID, remoteArchive, body)
		},
		Exec: func(execCtx context.Context, command string) error {
			return b.execShell(execCtx, transport, sandboxID, command)
		},
	})
}

func (b *backend) uploadArchive(ctx context.Context, transport sandboxTransport, sandboxID, remoteArchive string, body io.Reader) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("cloud-run-sandbox read archive: %w", err)
	}
	// Gateway writeFile preserves UTF-8 strings. Ship binary archives as base64
	// text, then decode in-sandbox before extract (handled by archive sync's
	// extract command when the remote path ends in .tgz — we write .tgz.b64).
	b64Path := remoteArchive + ".b64"
	encoded := base64.StdEncoding.EncodeToString(data)
	if err := transport.WriteFile(ctx, sandboxID, b64Path, encoded); err != nil {
		return err
	}
	decode := fmt.Sprintf("base64 -d %s > %s && rm -f %s", shellQuote(b64Path), shellQuote(remoteArchive), shellQuote(b64Path))
	return b.execShell(ctx, transport, sandboxID, decode)
}

func (b *backend) execShell(ctx context.Context, transport sandboxTransport, sandboxID, command string) error {
	code, err := transport.Exec(ctx, sandboxID, command, execOptions{
		Workdir: "/",
		Timeout: defaultExecTimeout,
	}, nil, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return exit(code, "cloud-run-sandbox exec %q exited %d", command, code)
	}
	return nil
}

func (b *backend) ensureWorkspace(ctx context.Context, transport sandboxTransport, sandboxID, workdir string) error {
	return b.execShell(ctx, transport, sandboxID, "mkdir -p "+shellQuote(workdir))
}

func (b *backend) execCommand(ctx context.Context, transport sandboxTransport, sandboxID, workdir string, command []string, env map[string]string, stdout, stderr io.Writer) (int, error) {
	if len(command) == 0 {
		return 2, exit(2, "missing command")
	}
	commandText := shellScriptFromArgv(command)
	if len(command) == 1 && shouldUseShell(command) {
		commandText = command[0]
	}
	return transport.Exec(ctx, sandboxID, commandText, execOptions{
		Workdir: workdir,
		Env:     env,
		Timeout: defaultExecTimeout,
	}, stdout, stderr)
}

func buildCommand(command []string, shellMode bool) ([]string, error) {
	if len(command) == 0 {
		return nil, exit(2, "missing command")
	}
	if shellMode {
		return []string{strings.Join(command, " ")}, nil
	}
	return command, nil
}
