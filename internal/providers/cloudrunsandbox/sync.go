package cloudrunsandbox

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const archiveUploadChunkSize = 3 << 20

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
	// Gateway writeFile preserves UTF-8 strings. Ship binary archives as base64
	// text in bounded chunks, then decode in-sandbox before extract. Chunking
	// avoids retaining the archive, its base64 expansion, and a JSON copy in
	// memory, while keeping every gateway request below ordinary HTTP limits.
	b64Path := remoteArchive + ".b64"
	chunks := &sandboxChunkWriter{
		ctx:       ctx,
		transport: transport,
		sandboxID: sandboxID,
		path:      b64Path,
		buffer:    make([]byte, 0, archiveUploadChunkSize),
	}
	encoder := base64.NewEncoder(base64.StdEncoding, chunks)
	_, copyErr := io.Copy(encoder, body)
	encodeErr := encoder.Close()
	flushErr := chunks.Close()
	if err := errors.Join(copyErr, encodeErr, flushErr); err != nil {
		return fmt.Errorf("cloud-run-sandbox upload archive: %w", err)
	}
	decode := fmt.Sprintf("base64 -d %s > %s && rm -f %s", shellQuote(b64Path), shellQuote(remoteArchive), shellQuote(b64Path))
	return b.execShell(ctx, transport, sandboxID, decode)
}

type sandboxChunkWriter struct {
	ctx       context.Context
	transport sandboxTransport
	sandboxID string
	path      string
	buffer    []byte
	append    bool
}

func (w *sandboxChunkWriter) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		space := archiveUploadChunkSize - len(w.buffer)
		if space > len(data) {
			space = len(data)
		}
		w.buffer = append(w.buffer, data[:space]...)
		data = data[space:]
		written += space
		if len(w.buffer) == archiveUploadChunkSize {
			if err := w.flush(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (w *sandboxChunkWriter) Close() error {
	if len(w.buffer) == 0 && w.append {
		return nil
	}
	return w.flush()
}

func (w *sandboxChunkWriter) flush() error {
	if err := w.transport.WriteFile(w.ctx, w.sandboxID, w.path, string(w.buffer), w.append); err != nil {
		return err
	}
	w.buffer = w.buffer[:0]
	w.append = true
	return nil
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
