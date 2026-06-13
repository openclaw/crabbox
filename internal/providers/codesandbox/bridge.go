package codesandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type BridgeRequest struct {
	Operation              string            `json:"operation"`
	Limit                  int               `json:"limit,omitempty"`
	SandboxID              string            `json:"sandboxId,omitempty"`
	Title                  string            `json:"title,omitempty"`
	Tags                   []string          `json:"tags,omitempty"`
	TemplateID             string            `json:"templateId,omitempty"`
	Privacy                string            `json:"privacy,omitempty"`
	VMTier                 string            `json:"vmTier,omitempty"`
	HibernationTimeoutSecs int               `json:"hibernationTimeoutSecs,omitempty"`
	AutomaticWakeupHTTP    bool              `json:"automaticWakeupHttp,omitempty"`
	AutomaticWakeupWS      bool              `json:"automaticWakeupWebSocket,omitempty"`
	Command                []string          `json:"command,omitempty"`
	Cwd                    string            `json:"cwd,omitempty"`
	Env                    map[string]string `json:"env,omitempty"`
	Timeout                int               `json:"timeout,omitempty"`
	Path                   string            `json:"path,omitempty"`
	ContentBase64          string            `json:"contentBase64,omitempty"`
	Encoding               string            `json:"encoding,omitempty"`
}

type BridgeResponse struct {
	OK         bool             `json:"ok"`
	Sandbox    SandboxSummary   `json:"sandbox,omitempty"`
	Sandboxes  []SandboxSummary `json:"sandboxes,omitempty"`
	TotalCount int              `json:"totalCount,omitempty"`
	Command    CommandResult    `json:"command,omitempty"`
	Error      *BridgeError     `json:"error,omitempty"`
}

type BridgeError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type SDKBridge struct {
	cfg CodeSandboxConfig
	rt  Runtime
}

func NewSDKBridge(cfg CodeSandboxConfig, rt Runtime) *SDKBridge {
	return &SDKBridge{cfg: cfg, rt: rt}
}

func (b *SDKBridge) RoundTrip(ctx context.Context, token string, req BridgeRequest) (BridgeResponse, error) {
	if b.rt.Exec == nil {
		return BridgeResponse{}, exit(2, "codesandbox bridge requires Runtime.Exec")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return BridgeResponse{}, err
	}
	ctx, cancel := withOperationTimeout(ctx, b.cfg)
	defer cancel()
	var stdout, stderr bytes.Buffer
	result, runErr := b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:                   bridgeCommand(b.cfg),
		Args:                   []string{"--input-type=module", "-e", codeSandboxBridgeScript},
		Env:                    bridgeEnv(b.cfg, token),
		Stdin:                  bytes.NewReader(payload),
		Stdout:                 &stdout,
		Stderr:                 &stderr,
		MaxCapturedOutputBytes: 1 << 20,
		CancelGracePeriod:      2 * time.Second,
	})
	if runErr != nil {
		return BridgeResponse{}, bridgeCommandError(result.ExitCode, stdout.String(), stderr.String(), token, runErr)
	}
	if result.ExitCode != 0 {
		return BridgeResponse{}, bridgeCommandError(result.ExitCode, stdout.String(), stderr.String(), token, nil)
	}
	data := bytes.TrimSpace(stdout.Bytes())
	if len(data) == 0 {
		return BridgeResponse{}, fmt.Errorf("codesandbox bridge returned empty JSON output")
	}
	var resp BridgeResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return BridgeResponse{}, fmt.Errorf("decode codesandbox bridge JSON: %w", err)
	}
	if !resp.OK {
		if resp.Error == nil {
			return BridgeResponse{}, fmt.Errorf("codesandbox bridge failed without error details")
		}
		return BridgeResponse{}, fmt.Errorf("codesandbox bridge %s: %s", blank(resp.Error.Code, "error"), redactToken(resp.Error.Message, token))
	}
	return resp, nil
}

func bridgeEnv(cfg CodeSandboxConfig, token string) []string {
	env := append([]string{}, os.Environ()...)
	env = upsertEnv(env, codesandboxFallbackAPIKeyEnv, token)
	env = upsertEnv(env, "CRABBOX_CODESANDBOX_SDK_PACKAGE", sdkPackage(cfg))
	return env
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func bridgeCommandError(exitCode int, stdout, stderr, token string, err error) error {
	message := strings.TrimSpace(stderr)
	if message == "" {
		message = strings.TrimSpace(stdout)
	}
	if message == "" && err != nil {
		message = err.Error()
	}
	if message == "" {
		message = "no output"
	}
	message = redactToken(message, token)
	if err != nil {
		return fmt.Errorf("codesandbox bridge failed exit=%d: %s: %w", exitCode, message, err)
	}
	return fmt.Errorf("codesandbox bridge failed exit=%d: %s", exitCode, message)
}

const codeSandboxBridgeScript = `
const chunks = [];
for await (const chunk of process.stdin) chunks.push(chunk);
const input = Buffer.concat(chunks).toString("utf8").trim();
const req = input ? JSON.parse(input) : {};
function emit(value) {
  process.stdout.write(JSON.stringify(value));
}
function fail(code, message) {
  emit({ ok: false, error: { code, message: String(message || "") } });
}
function normalizeSandbox(sandbox) {
  if (!sandbox) return {};
  const tags = Array.isArray(sandbox.tags) ? sandbox.tags : [];
  return {
    id: sandbox.id || sandbox.sandboxId || sandbox.uid || "",
    title: sandbox.title || sandbox.name || "",
    privacy: sandbox.privacy || "",
    tags,
    state: sandbox.state || sandbox.status || sandbox.lifecycleStatus || "",
    url: sandbox.url || sandbox.editorUrl || sandbox.previewUrl || ""
  };
}
async function callAny(target, names, ...args) {
  for (const name of names) {
    if (target && typeof target[name] === "function") {
      return await target[name](...args);
    }
  }
  throw new Error("CodeSandbox SDK does not expose any of: " + names.join(", "));
}
async function openSandbox(sdk, id) {
  const sandboxes = sdk.sandboxes || sdk;
  return await callAny(sandboxes, ["get", "connect", "open", "resume"], id);
}
async function createSandbox(sdk) {
  const sandboxes = sdk.sandboxes || sdk;
  const options = {};
  if (req.title) options.title = req.title;
  if (Array.isArray(req.tags) && req.tags.length) options.tags = req.tags;
  if (req.templateId) options.template = req.templateId;
  if (req.templateId) options.templateId = req.templateId;
  if (req.privacy) options.privacy = req.privacy;
  if (req.vmTier) options.vmTier = req.vmTier;
  if (req.hibernationTimeoutSecs) options.hibernateAfterSeconds = Number(req.hibernationTimeoutSecs);
  options.automaticWakeup = {
    http: !!req.automaticWakeupHttp,
    websocket: !!req.automaticWakeupWebSocket
  };
  return await callAny(sandboxes, ["create", "createSandbox"], options);
}
async function runCommand(sandbox) {
  const command = Array.isArray(req.command) ? req.command.filter((v) => String(v || "") !== "") : [];
  if (!command.length) throw new Error("missing command");
  const cwd = req.cwd || "/project/workspace";
  const env = req.env || {};
  const timeout = Number(req.timeout || 0) || undefined;
  const commands = sandbox.commands || sandbox.command || sandbox;
  let result;
  if (commands && typeof commands.run === "function") {
    result = await commands.run(command[0], command.slice(1), { cwd, env, timeout });
  } else {
    result = await callAny(commands, ["exec", "runCommand"], {
      command: command[0],
      cmd: command[0],
      args: command.slice(1),
      cwd,
      env,
      timeout
    });
  }
  return {
    exitCode: Number(result && (result.exitCode ?? result.code ?? result.status) || 0),
    stdout: String(result && (result.stdout ?? result.output ?? "") || ""),
    stderr: String(result && (result.stderr ?? result.errorOutput ?? "") || "")
  };
}
async function writeFile(sandbox) {
  const files = sandbox.filesystem || sandbox.fs || sandbox.files || sandbox;
  const buffer = Buffer.from(String(req.contentBase64 || ""), "base64");
  if (files && typeof files.writeFile === "function") {
    await files.writeFile(req.path, buffer);
    return;
  }
  if (files && typeof files.write === "function") {
    await files.write(req.path, buffer);
    return;
  }
  await callAny(files, ["writeTextFile", "createFile"], req.path, buffer);
}
try {
  const token = process.env.CSB_API_KEY || process.env.CRABBOX_CODESANDBOX_API_KEY || "";
  if (!token) {
    fail("auth_missing", "missing CodeSandbox API key");
  } else {
    const pkg = process.env.CRABBOX_CODESANDBOX_SDK_PACKAGE || "@codesandbox/sdk";
    const { CodeSandbox } = await import(pkg);
    const sdk = new CodeSandbox(token);
    if (req.operation === "list_sandboxes") {
      const listed = await callAny(sdk.sandboxes || sdk, ["list", "listSandboxes"], { limit: Number(req.limit || 1) });
      const items = listed.sandboxes || listed.items || listed.results || listed || [];
      const sandboxes = Array.from(items).map(normalizeSandbox);
      emit({ ok: true, sandboxes, totalCount: listed.totalCount || listed.total || sandboxes.length });
    } else if (req.operation === "create_sandbox") {
      const sandbox = await createSandbox(sdk);
      emit({ ok: true, sandbox: normalizeSandbox(sandbox) });
    } else if (req.operation === "get_sandbox") {
      const sandbox = await openSandbox(sdk, req.sandboxId);
      emit({ ok: true, sandbox: normalizeSandbox(sandbox) });
    } else if (req.operation === "delete_sandbox") {
      await callAny(sdk.sandboxes || sdk, ["delete", "deleteSandbox"], req.sandboxId);
      emit({ ok: true });
    } else if (req.operation === "run_command") {
      const sandbox = await openSandbox(sdk, req.sandboxId);
      const command = await runCommand(sandbox);
      emit({ ok: true, command });
    } else if (req.operation === "write_file") {
      const sandbox = await openSandbox(sdk, req.sandboxId);
      await writeFile(sandbox);
      emit({ ok: true });
    } else {
      fail("unsupported_operation", "unsupported bridge operation: " + String(req.operation || ""));
    }
  }
} catch (err) {
  fail(err && err.code ? err.code : "sdk_error", err && err.message ? err.message : err);
}
`

func blank(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
