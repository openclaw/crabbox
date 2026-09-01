package islo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	core "github.com/openclaw/crabbox/internal/cli"
)

// isloScriptEpoch is a fixed archive timestamp so that packing the same script
// twice produces identical bytes.
var isloScriptEpoch = time.Unix(0, 0).UTC()

// isloScriptNormalizePath maps a spec path onto the slash-separated,
// lexically-clean form used both inside the archive and in the remote argv, so
// upload and removal can never disagree about which file they mean.
func isloScriptNormalizePath(raw string) string {
	return path.Clean(strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/"))
}

// isloScriptRemotePath validates that the script lands inside the sandbox
// workspace. It is the single gate for the path: nothing uploads, executes or
// removes a path that has not passed through here, which is what keeps a
// cleanup `rm` from ever being handed an absolute or escaping path.
func isloScriptRemotePath(spec *core.RunScriptSpec) (string, error) {
	if spec == nil {
		return "", fmt.Errorf("islo run script is missing")
	}
	remote := isloScriptNormalizePath(spec.RemotePath)
	if remote == "" || remote == "." || path.IsAbs(remote) || remote == ".." || strings.HasPrefix(remote, "../") {
		return "", fmt.Errorf("islo run script path %q is not a workspace-relative path", spec.RemotePath)
	}
	return remote, nil
}

// isloScriptArchive packs a run script into a gzipped tar so it can be placed
// through the Islo files-archive endpoint. The script bytes are carried
// verbatim: they are never interpolated into a shell command string, so
// multiline and binary-ish payloads survive intact.
func isloScriptArchive(spec *core.RunScriptSpec) (io.Reader, error) {
	if spec == nil || len(spec.Data) == 0 {
		return nil, fmt.Errorf("islo run script is empty")
	}
	remote, err := isloScriptRemotePath(spec)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: remote,
		Mode: 0o700,
		Size: int64(len(spec.Data)),
		// A fixed epoch keeps the archive byte-identical across replays of the
		// same script, so a retried warmup does not look like a new payload.
		ModTime: isloScriptEpoch,
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(spec.Data); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}

// isloScriptCommand builds the argv that executes an uploaded script. The
// remote path is passed as a positional argument rather than spliced into the
// script text, which is what keeps arbitrary paths from being reinterpreted by
// the shell. This mirrors the SSH-backed contract in internal/cli/run_script.go.
func isloScriptCommand(spec *core.RunScriptSpec, args []string) []string {
	remote := isloScriptNormalizePath(spec.RemotePath)
	inner := `exec bash "$@"`
	if spec.Shebang {
		inner = `exec "$@"`
	}
	argv := []string{"bash", "-lc", inner, "bash", remote}
	return append(argv, args...)
}

// uploadRunScript places the script inside the sandbox workspace. The first
// result reports whether the upload was actually issued to the provider: when
// it is true the script may exist remotely even if err is non-nil (a transport
// failure can still leave an extracted file behind), so the caller must run
// cleanup. When it is false the path never passed validation and no request
// was made, so there is nothing to remove and no path safe to hand to `rm`.
func (b *isloBackend) uploadRunScript(ctx context.Context, client isloAPI, name, workspace string, spec *core.RunScriptSpec) (bool, error) {
	archive, err := isloScriptArchive(spec)
	if err != nil {
		return false, err
	}
	if err := client.UploadArchive(ctx, name, workspace, archive); err != nil {
		return true, fmt.Errorf("islo upload run script %s: %w", spec.RemotePath, err)
	}
	return true, nil
}

// removeRunScript deletes the uploaded script. Cleanup is best effort: the
// script has already run by this point, and a delete failure must not change
// the exit code the caller observes.
//
// It deliberately does not take the workload context. Removal runs from a
// deferred call that is reached precisely when the run ended early - including
// when the run context was cancelled or hit its deadline - and ExecStream binds
// its context to the HTTP request, so inheriting that context would mean the
// removal request is never sent and the uploaded script stays in a kept or
// reused sandbox. A fresh bounded context, the same primitive sandbox deletion
// uses, keeps cleanup reachable while still being unable to hang forever.
func (b *isloBackend) removeRunScript(client isloAPI, name, workspace string, spec *core.RunScriptSpec, user string) {
	remote, err := isloScriptRemotePath(spec)
	if err != nil {
		// Unreachable via runScript, which only schedules cleanup for a
		// validated path. Refusing to build an `rm` argv from an unvalidated
		// path keeps that a local invariant rather than a caller contract.
		fmt.Fprintf(b.rt.Stderr, "warning: islo skipped run script cleanup: %v\n", err)
		return
	}
	ctx, cancel := isloCleanupContext()
	defer cancel()
	req := &gosdk.ExecRequest{Command: []string{"bash", "-lc", `rm -f -- "$1"`, "bash", remote}}
	if user != "" {
		req.User = stringValue(user)
	}
	if workspace != "" {
		req.Workdir = stringValue(workspace)
	}
	if _, err := client.ExecStream(ctx, name, req, io.Discard, io.Discard); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: islo could not remove run script %s: %v\n", remote, err)
	}
}

// runScript uploads, executes and removes a delegated POSIX run script,
// returning the script's exact exit code.
func (b *isloBackend) runScript(ctx context.Context, client isloAPI, name, workspace string, req RunRequest, env map[string]string, user string) (int, error) {
	uploaded, err := b.uploadRunScript(ctx, client, name, workspace, req.Script)
	if uploaded {
		defer b.removeRunScript(client, name, workspace, req.Script, user)
	}
	if err != nil {
		return 7, err
	}
	execReq := &gosdk.ExecRequest{Command: isloScriptCommand(req.Script, req.Command)}
	if user != "" {
		execReq.User = stringValue(user)
	}
	if workspace != "" {
		execReq.Workdir = stringValue(workspace)
	}
	if len(env) > 0 {
		execReq.Env = make(map[string]*string, len(env))
		for key, value := range env {
			value := value
			execReq.Env[key] = &value
		}
	}
	return client.ExecStream(ctx, name, execReq, b.rt.Stdout, b.rt.Stderr)
}
