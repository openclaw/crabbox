package modal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

type modalAPI interface {
	CreateSandbox(context.Context, modalCreateSandboxRequest) (modalSandbox, error)
	Exec(context.Context, modalExecRequest) (int, error)
	UploadFile(context.Context, string, string, string) error
	GetSandbox(context.Context, string) (modalSandbox, error)
	ListSandboxes(context.Context, map[string]string) ([]modalSandbox, error)
	Terminate(context.Context, string) error
}

type modalCreateSandboxRequest struct {
	App            string            `json:"app"`
	Image          string            `json:"image"`
	Workdir        string            `json:"workdir"`
	Name           string            `json:"name,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	Tags           map[string]string `json:"tags"`
}

type modalExecRequest struct {
	SandboxID string
	Command   []string
	Timeout   int
	Stdout    io.Writer
	Stderr    io.Writer
}

type modalSandbox struct {
	ID     string            `json:"id"`
	Name   string            `json:"name,omitempty"`
	Status string            `json:"status,omitempty"`
	Tags   map[string]string `json:"tags,omitempty"`
}

type modalPythonClient struct {
	cfg Config
	rt  Runtime
}

var newModalAPI = func(cfg Config, rt Runtime) (modalAPI, error) {
	if rt.Exec == nil {
		return nil, exit(2, "provider=modal requires Runtime.Exec")
	}
	return &modalPythonClient{cfg: cfg, rt: rt}, nil
}

const modalTransportExitCode = 125

func (c *modalPythonClient) CreateSandbox(ctx context.Context, req modalCreateSandboxRequest) (modalSandbox, error) {
	var sandbox modalSandbox
	if err := c.runJSON(ctx, modalCreateScript, req, &sandbox); err != nil {
		return modalSandbox{}, err
	}
	if sandbox.Tags == nil {
		sandbox.Tags = req.Tags
	}
	return sandbox, nil
}

func (c *modalPythonClient) Exec(ctx context.Context, req modalExecRequest) (int, error) {
	if req.Stdout == nil {
		req.Stdout = io.Discard
	}
	if req.Stderr == nil {
		req.Stderr = io.Discard
	}
	payload := map[string]any{
		"sandbox_id": req.SandboxID,
		"command":    req.Command,
		"timeout":    req.Timeout,
	}
	resultFile, err := os.CreateTemp("", "crabbox-modal-exec-*.rc")
	if err != nil {
		return 0, fmt.Errorf("create modal exec result file: %w", err)
	}
	resultPath := resultFile.Name()
	_ = resultFile.Close()
	defer os.Remove(resultPath)
	payload["result_path"] = resultPath
	res, err := c.runStreamed(ctx, modalExecScript, payload, req.Stdout, req.Stderr)
	if err != nil {
		return res.ExitCode, err
	}
	if res.ExitCode != 0 {
		if res.ExitCode == modalTransportExitCode {
			return res.ExitCode, fmt.Errorf("modal exec transport failed")
		}
		return res.ExitCode, fmt.Errorf("modal exec client exited %d", res.ExitCode)
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return 0, fmt.Errorf("read modal exec result: %w", err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("decode modal exec result %q: %w", strings.TrimSpace(string(data)), err)
	}
	return code, nil
}

func (c *modalPythonClient) UploadFile(ctx context.Context, sandboxID, localPath, remotePath string) error {
	payload := map[string]any{
		"sandbox_id":  sandboxID,
		"local_path":  localPath,
		"remote_path": remotePath,
	}
	res, err := c.runStreamed(ctx, modalUploadScript, payload, io.Discard, c.rt.Stderr)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return exit(res.ExitCode, "modal upload %q exited %d", remotePath, res.ExitCode)
	}
	return nil
}

func (c *modalPythonClient) GetSandbox(ctx context.Context, sandboxID string) (modalSandbox, error) {
	var sandbox modalSandbox
	if err := c.runJSON(ctx, modalGetScript, map[string]string{"sandbox_id": sandboxID}, &sandbox); err != nil {
		return modalSandbox{}, err
	}
	return sandbox, nil
}

func (c *modalPythonClient) ListSandboxes(ctx context.Context, tags map[string]string) ([]modalSandbox, error) {
	payload := map[string]any{
		"app":  c.app(),
		"tags": tags,
	}
	var sandboxes []modalSandbox
	if err := c.runJSON(ctx, modalListScript, payload, &sandboxes); err != nil {
		return nil, err
	}
	return sandboxes, nil
}

func (c *modalPythonClient) Terminate(ctx context.Context, sandboxID string) error {
	res, err := c.runStreamed(ctx, modalTerminateScript, map[string]string{"sandbox_id": sandboxID}, io.Discard, c.rt.Stderr)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return exit(res.ExitCode, "modal terminate %s exited %d", sandboxID, res.ExitCode)
	}
	return nil
}

func (c *modalPythonClient) runJSON(ctx context.Context, script string, payload any, out any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var stdout, stderr bytes.Buffer
	res, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   c.python(),
		Args:   []string{"-c", script, string(data)},
		Env:    c.env(),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil || res.ExitCode != 0 {
		return modalCommandError(res.ExitCode, &stdout, &stderr, err)
	}
	line := lastNonEmptyLine(stdout.String())
	if line == "" {
		return fmt.Errorf("modal python client returned empty JSON output")
	}
	if err := json.Unmarshal([]byte(line), out); err != nil {
		return fmt.Errorf("decode modal python client JSON %q: %w", line, err)
	}
	return nil
}

func (c *modalPythonClient) runStreamed(ctx context.Context, script string, payload any, stdout, stderr io.Writer) (coreResult, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return coreResult{}, err
	}
	res, err := c.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:   c.python(),
		Args:   []string{"-c", script, string(data)},
		Env:    c.env(),
		Stdout: stdout,
		Stderr: stderr,
	})
	if err != nil && res.ExitCode == 0 {
		return coreResult{ExitCode: res.ExitCode}, err
	}
	return coreResult{ExitCode: res.ExitCode}, nil
}

type coreResult struct {
	ExitCode int
}

func (c *modalPythonClient) python() string {
	return blank(strings.TrimSpace(c.cfg.Modal.Python), "python3")
}

func (c *modalPythonClient) app() string {
	return blank(strings.TrimSpace(c.cfg.Modal.App), "crabbox")
}

func (c *modalPythonClient) env() []string {
	return os.Environ()
}

func modalCommandError(exitCode int, stdout, stderr *bytes.Buffer, runErr error) error {
	tail := strings.TrimSpace(stderr.String())
	if tail == "" {
		tail = strings.TrimSpace(stdout.String())
	}
	if len(tail) > 4096 {
		tail = tail[:4096]
	}
	if runErr != nil {
		return fmt.Errorf("modal python client (exit=%d): %v: %s", exitCode, runErr, tail)
	}
	return fmt.Errorf("modal python client exited %d: %s", exitCode, tail)
}

func lastNonEmptyLine(value string) string {
	lines := strings.Split(value, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

const modalPythonPrelude = `
import json
import os
import sys
import threading
import traceback

TRANSPORT_EXIT = 125

def fail(exc):
    print("modal python client: %s" % exc, file=sys.stderr)
    sys.exit(TRANSPORT_EXIT)

def load_payload():
    return json.loads(sys.argv[1])

def sandbox_id(sb):
    return (
        getattr(sb, "object_id", None)
        or getattr(sb, "sandbox_id", None)
        or getattr(sb, "sandbox_id", "")
    )

def sandbox_tags(sb):
    try:
        tags = sb.get_tags()
        return tags or {}
    except Exception:
        return {}

def sandbox_status(sb):
    try:
        rc = sb.poll()
        return "running" if rc is None else "finished"
    except Exception:
        return "running"

def sandbox_json(sb):
    return {
        "id": sandbox_id(sb),
        "name": getattr(sb, "name", "") or "",
        "status": sandbox_status(sb),
        "tags": sandbox_tags(sb),
    }

def write_stream(src, dst):
    try:
        for chunk in src:
            if isinstance(chunk, str):
                chunk = chunk.encode()
            dst.write(chunk)
            dst.flush()
    except Exception as exc:
        print("modal stream copy failed: %s" % exc, file=sys.stderr)
`

const modalCreateScript = modalPythonPrelude + `
try:
    req = load_payload()
    import modal
    app = modal.App.lookup(req["app"], create_if_missing=True)
    image_name = req.get("image") or "python:3.13-slim"
    image = modal.Image.from_registry(image_name)
    kwargs = {
        "app": app,
        "image": image,
        "timeout": int(req.get("timeout_seconds") or 300),
    }
    if req.get("workdir"):
        kwargs["workdir"] = req["workdir"]
    if req.get("name"):
        kwargs["name"] = req["name"]
    sb = modal.Sandbox.create(**kwargs)
    tags = req.get("tags") or {}
    if tags:
        sb.set_tags(tags)
    out = sandbox_json(sb)
    out["tags"] = tags
    try:
        sb.detach()
    except Exception:
        pass
    print(json.dumps(out, sort_keys=True))
except Exception as exc:
    fail(exc)
`

const modalExecScript = modalPythonPrelude + `
try:
    req = load_payload()
    import modal
    sb = modal.Sandbox.from_id(req["sandbox_id"])
    command = req.get("command") or []
    timeout = int(req.get("timeout") or 0)
    kwargs = {}
    if timeout > 0:
        kwargs["timeout"] = timeout
    proc = sb.exec(*command, **kwargs)
    threads = [
        threading.Thread(target=write_stream, args=(proc.stdout, sys.stdout.buffer)),
        threading.Thread(target=write_stream, args=(proc.stderr, sys.stderr.buffer)),
    ]
    for thread in threads:
        thread.daemon = True
        thread.start()
    rc = proc.wait()
    for thread in threads:
        thread.join()
    result_path = req.get("result_path")
    if result_path:
        with open(result_path, "w", encoding="utf-8") as f:
            f.write(str(0 if rc is None else int(rc)))
    else:
        sys.exit(0 if rc is None else int(rc))
except Exception as exc:
    fail(exc)
`

const modalUploadScript = modalPythonPrelude + `
try:
    req = load_payload()
    import modal
    sb = modal.Sandbox.from_id(req["sandbox_id"])
    remote_path = req["remote_path"]
    remote_dir = os.path.dirname(remote_path) or "/tmp"
    sb.filesystem.make_directory(remote_dir, create_parents=True)
    sb.filesystem.copy_from_local(req["local_path"], remote_path)
except Exception as exc:
    fail(exc)
`

const modalGetScript = modalPythonPrelude + `
try:
    req = load_payload()
    import modal
    sb = modal.Sandbox.from_id(req["sandbox_id"])
    print(json.dumps(sandbox_json(sb), sort_keys=True))
except Exception as exc:
    fail(exc)
`

const modalListScript = modalPythonPrelude + `
try:
    req = load_payload()
    import modal
    app = modal.App.lookup(req["app"], create_if_missing=True)
    items = []
    for sb in modal.Sandbox.list(app_id=app.app_id, tags=req.get("tags") or {}):
        items.append(sandbox_json(sb))
    print(json.dumps(items, sort_keys=True))
except Exception as exc:
    fail(exc)
`

const modalTerminateScript = modalPythonPrelude + `
try:
    req = load_payload()
    import modal
    sb = modal.Sandbox.from_id(req["sandbox_id"])
    sb.terminate()
    try:
        sb.detach()
    except Exception:
        pass
except Exception as exc:
    fail(exc)
`
