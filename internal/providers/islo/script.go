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

// isloScriptArchive packs a run script into a gzipped tar so it can be placed
// through the Islo files-archive endpoint. The script bytes are carried
// verbatim: they are never interpolated into a shell command string, so
// multiline and binary-ish payloads survive intact.
func isloScriptArchive(spec *core.RunScriptSpec) (io.Reader, error) {
	if spec == nil || len(spec.Data) == 0 {
		return nil, fmt.Errorf("islo run script is empty")
	}
	remote := path.Clean(strings.ReplaceAll(strings.TrimSpace(spec.RemotePath), "\\", "/"))
	if remote == "" || remote == "." || path.IsAbs(remote) || remote == ".." || strings.HasPrefix(remote, "../") {
		return nil, fmt.Errorf("islo run script path %q is not a workspace-relative path", spec.RemotePath)
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
	remote := path.Clean(strings.ReplaceAll(strings.TrimSpace(spec.RemotePath), "\\", "/"))
	inner := `exec bash "$@"`
	if spec.Shebang {
		inner = `exec "$@"`
	}
	argv := []string{"bash", "-lc", inner, "bash", remote}
	return append(argv, args...)
}

// uploadRunScript places the script inside the sandbox workspace.
func (b *isloBackend) uploadRunScript(ctx context.Context, client isloAPI, name, workspace string, spec *core.RunScriptSpec) error {
	archive, err := isloScriptArchive(spec)
	if err != nil {
		return err
	}
	if err := client.UploadArchive(ctx, name, workspace, archive); err != nil {
		return fmt.Errorf("islo upload run script %s: %w", spec.RemotePath, err)
	}
	return nil
}

// removeRunScript deletes the uploaded script. Cleanup is best effort: the
// script has already run by this point, and a delete failure must not change
// the exit code the caller observes.
func (b *isloBackend) removeRunScript(ctx context.Context, client isloAPI, name, workspace string, spec *core.RunScriptSpec, user string) {
	remote := path.Clean(strings.ReplaceAll(strings.TrimSpace(spec.RemotePath), "\\", "/"))
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
	if err := b.uploadRunScript(ctx, client, name, workspace, req.Script); err != nil {
		return 7, err
	}
	defer b.removeRunScript(ctx, client, name, workspace, req.Script, user)
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
