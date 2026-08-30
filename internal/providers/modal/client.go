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
	Bind(modalBinding) modalAPI
	CreateSandbox(context.Context, modalCreateSandboxRequest) (modalSandbox, error)
	InspectSandbox(context.Context, modalBinding) (modalSandbox, error)
	Exec(context.Context, modalExecRequest) (int, error)
	UploadFile(context.Context, string, string, string) error
	GetSandbox(context.Context, string) (modalSandbox, error)
	ListSandboxes(context.Context, map[string]string) ([]modalSandbox, error)
	Terminate(context.Context, modalBinding) error
}

type modalCreateSandboxRequest struct {
	App            string            `json:"app"`
	Image          string            `json:"image"`
	Workdir        string            `json:"workdir"`
	Name           string            `json:"name,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	Tags           map[string]string `json:"tags"`
	Environment    string            `json:"environment,omitempty"`
	Secrets        []string          `json:"secrets,omitempty"`
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
	Scope  modalScope        `json:"scope"`
}

type modalPythonClient struct {
	cfg     Config
	rt      Runtime
	binding *modalBinding
}

var newModalAPI = func(cfg Config, rt Runtime) (modalAPI, error) {
	if rt.Exec == nil {
		return nil, exit(2, "provider=modal requires Runtime.Exec")
	}
	return &modalPythonClient{cfg: cfg, rt: rt}, nil
}

const modalTransportExitCode = 125

func (c *modalPythonClient) Bind(binding modalBinding) modalAPI {
	bound := *c
	bound.binding = &binding
	return &bound
}

func (c *modalPythonClient) sandboxPayload(id string) map[string]any {
	return map[string]any{"sandbox_id": id, "binding": c.binding, "app": c.app(), "environment": strings.TrimSpace(c.cfg.Modal.Environment)}
}

func (c *modalPythonClient) InspectSandbox(ctx context.Context, binding modalBinding) (modalSandbox, error) {
	bound := c.Bind(binding).(*modalPythonClient)
	return bound.GetSandbox(ctx, binding.ID)
}

func (c *modalPythonClient) CreateSandbox(ctx context.Context, req modalCreateSandboxRequest) (modalSandbox, error) {
	var sandbox modalSandbox
	if err := c.runJSON(ctx, modalCreateScript, req, &sandbox); err != nil {
		return modalSandbox{}, err
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
	payload := c.sandboxPayload(req.SandboxID)
	payload["command"], payload["timeout"] = req.Command, req.Timeout
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
	payload := c.sandboxPayload(sandboxID)
	payload["local_path"], payload["remote_path"] = localPath, remotePath
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
	if err := c.runJSON(ctx, modalGetScript, c.sandboxPayload(sandboxID), &sandbox); err != nil {
		return modalSandbox{}, err
	}
	return sandbox, nil
}

func (c *modalPythonClient) ListSandboxes(ctx context.Context, tags map[string]string) ([]modalSandbox, error) {
	payload := map[string]any{
		"app":         c.app(),
		"environment": strings.TrimSpace(c.cfg.Modal.Environment),
		"tags":        tags,
	}
	var sandboxes []modalSandbox
	if err := c.runJSON(ctx, modalListScript, payload, &sandboxes); err != nil {
		return nil, err
	}
	return sandboxes, nil
}

func (c *modalPythonClient) Terminate(ctx context.Context, binding modalBinding) error {
	bound := c.Bind(binding).(*modalPythonClient)
	var sandbox modalSandbox
	if err := bound.runJSON(ctx, modalTerminateScript, bound.sandboxPayload(binding.ID), &sandbox); err != nil {
		return err
	}
	if err := binding.validate(sandbox); err != nil {
		return err
	}
	if sandbox.Status != "finished" {
		return exit(5, "Modal termination did not confirm terminal state")
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
_stream_errors = []
_stream_errors_lock = threading.Lock()

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
    return sb.get_tags() or {}

def sandbox_status(sb):
    return "running" if sb.poll() is None else "finished"

def sandbox_json(sb):
    return {
        "id": sandbox_id(sb),
        "name": getattr(sb, "name", "") or "",
        "status": sandbox_status(sb),
        "tags": sandbox_tags(sb),
    }

def modal_context(req, create=False):
    import modal
    from modal.config import config
    # Capture routing before the first client lookup in this fresh child.
    endpoint = config.get("server_url")
    client = modal.Client.from_env()
    workspace = modal.Workspace.from_context(client=client)
    workspace.hydrate()
    if req.get("environment"):
        environment = modal.Environment.from_name(req["environment"], create_if_missing=False, client=client)
    else:
        environment = modal.Environment.from_context(client=client)
    environment.hydrate()
    if not environment.name or not workspace.name:
        raise RuntimeError("Modal returned incomplete workspace/environment identity")
    os.environ["MODAL_ENVIRONMENT"] = environment.name
    app = modal.App.lookup(req["app"], client=client, environment_name=environment.name, create_if_missing=create)
    scope = {"endpoint": endpoint, "workspace": workspace.name, "environment_id": environment.object_id,
             "environment": environment.name, "app_id": app.app_id, "app": req["app"]}
    return client, app, scope

def resolve_sandbox(req):
    import modal
    binding = req.get("binding")
    if not binding:
        # Unclaimed identifiers are allowed only for read-only discovery.
        sb = modal.Sandbox.from_id(req["sandbox_id"])
        return sb, sandbox_json(sb)
    client, app, scope = modal_context(req)
    if scope != binding["scope"] or req["sandbox_id"] != binding["id"]:
        raise RuntimeError("Modal authority or sandbox identity changed; retaining resource")
    sb = modal.Sandbox.from_id(binding["id"], client=client)
    out = sandbox_json(sb)
    out["scope"] = scope
    if out["id"] != binding["id"]:
        raise RuntimeError("Modal returned a different sandbox ID")
    expected = {"provider": "modal", "crabbox": "true", "lease": binding["lease"],
                "slug": binding["slug"], "app": scope["app"]}
    if any(out["tags"].get(k) != v for k, v in expected.items()):
        raise RuntimeError("Modal ownership tags changed; retaining resource")
    if out["status"] == "running":
        matches = [item for item in modal.Sandbox.list(app_id=app.app_id, tags=expected, client=client)
                   if sandbox_id(item) == binding["id"]]
        if len(matches) != 1:
            raise RuntimeError("Modal sandbox is not uniquely present in the bound app")
    return sb, out

def running_sandbox(req):
    if not req.get("binding"):
        import modal
        return modal.Sandbox.from_id(req["sandbox_id"])
    sb, out = resolve_sandbox(req)
    if req.get("binding") and out["status"] != "running":
        raise RuntimeError("Modal sandbox has finished; create a new lease")
    return sb

def write_stream(src, dst):
    try:
        for chunk in src:
            if isinstance(chunk, str):
                chunk = chunk.encode()
            dst.write(chunk)
            dst.flush()
    except Exception as exc:
        with _stream_errors_lock:
            _stream_errors.append(str(exc))
        print("modal stream copy failed: %s" % exc, file=sys.stderr)

def stream_error():
    with _stream_errors_lock:
        return "; ".join(_stream_errors)
`

const modalCreateScript = modalPythonPrelude + `
try:
    req = load_payload()
    import modal
    client, app, scope = modal_context(req, create=True)
    image_name = req.get("image") or "python:3.13-slim"
    image = modal.Image.from_registry(image_name)
    kwargs = {
        "app": app,
        "image": image,
        "timeout": int(req.get("timeout_seconds") or 300),
        "client": client,
        "tags": req.get("tags") or {},
    }
    if req.get("workdir"):
        kwargs["workdir"] = req["workdir"]
    if req.get("name"):
        kwargs["name"] = req["name"]
    if req.get("secrets"):
        kwargs["secrets"] = [
            modal.Secret.from_name(name, environment_name=scope["environment"])
            for name in req["secrets"]
        ]
    sb = modal.Sandbox.create(**kwargs)
    out = sandbox_json(sb)
    out["scope"] = scope
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
    sb = running_sandbox(req)
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
    copy_error = stream_error()
    if copy_error:
        fail("stream copy failed: %s" % copy_error)
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
    sb = running_sandbox(req)
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
    sb, out = resolve_sandbox(req)
    print(json.dumps(out, sort_keys=True))
except Exception as exc:
    fail(exc)
`

const modalListScript = modalPythonPrelude + `
try:
    req = load_payload()
    import modal
    client, app, scope = modal_context(req)
    items = []
    for sb in modal.Sandbox.list(app_id=app.app_id, tags=req.get("tags") or {}, client=client):
        items.append(sandbox_json(sb))
    print(json.dumps(items, sort_keys=True))
except Exception as exc:
    fail(exc)
`

const modalTerminateScript = modalPythonPrelude + `
try:
    req = load_payload()
    import modal
    if not req.get("binding"):
        raise RuntimeError("Modal termination requires an exact ownership binding")
    sb, out = resolve_sandbox(req)
    if out["status"] == "running":
        sb.terminate(wait=True)
        out = sandbox_json(sb)
        out["scope"] = req["binding"]["scope"]
    if out["status"] != "finished":
        raise RuntimeError("Modal termination remains unconfirmed")
    print(json.dumps(out, sort_keys=True))
    try:
        sb.detach()
    except Exception:
        pass
except Exception as exc:
    fail(exc)
`
