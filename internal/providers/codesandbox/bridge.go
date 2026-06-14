package codesandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	codeSandboxBridgeOutputLimit     = 1 << 20
	codeSandboxRunCommandOutputLimit = 64 << 20
	codeSandboxBridgeCacheDirEnv     = "CRABBOX_CODESANDBOX_BRIDGE_CACHE_DIR"
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
	Port                   int               `json:"port,omitempty"`
}

type BridgeResponse struct {
	OK         bool             `json:"ok"`
	Sandbox    SandboxSummary   `json:"sandbox,omitempty"`
	Sandboxes  []SandboxSummary `json:"sandboxes,omitempty"`
	TotalCount int              `json:"totalCount,omitempty"`
	Command    CommandResult    `json:"command,omitempty"`
	Port       PortInfo         `json:"port,omitempty"`
	Ports      []PortInfo       `json:"ports,omitempty"`
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
	dir, err := bridgeWorkingDir()
	if err != nil {
		return BridgeResponse{}, err
	}
	spec := bridgeSDKSpecFor(b.cfg)
	if err := b.ensureBridgeSDK(ctx, dir, spec); err != nil {
		return BridgeResponse{}, err
	}
	ctx, cancel := withBridgeTimeout(ctx, b.cfg, req)
	defer cancel()
	var stdout, stderr bytes.Buffer
	result, runErr := b.rt.Exec.Run(ctx, LocalCommandRequest{
		Name:                   bridgeCommand(b.cfg),
		Args:                   []string{"--input-type=module", "-e", codeSandboxBridgeScript},
		Env:                    bridgeEnv(b.cfg, token, spec.ImportSpec),
		Dir:                    dir,
		Stdin:                  bytes.NewReader(payload),
		Stdout:                 &stdout,
		Stderr:                 &stderr,
		MaxCapturedOutputBytes: bridgeOutputLimit(req),
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

func withBridgeTimeout(ctx context.Context, cfg CodeSandboxConfig, req BridgeRequest) (context.Context, context.CancelFunc) {
	timeout := operationTimeout(cfg)
	if req.Operation == "run_command" && req.Timeout > 0 {
		commandTimeout := time.Duration(req.Timeout+10) * time.Second
		if commandTimeout > timeout {
			timeout = commandTimeout
		}
	}
	return context.WithTimeout(ctx, timeout)
}

func (b *SDKBridge) ensureBridgeSDK(ctx context.Context, dir string, spec bridgeSDKSpec) error {
	if !spec.Install {
		return nil
	}
	if bridgeSDKInstalled(dir, spec) {
		return nil
	}
	if err := writeBridgePackageJSON(dir); err != nil {
		return err
	}
	setupCtx, cancel := context.WithTimeout(ctx, operationTimeout(b.cfg))
	defer cancel()
	var stdout, stderr bytes.Buffer
	result, runErr := b.rt.Exec.Run(setupCtx, LocalCommandRequest{
		Name:                   "npm",
		Args:                   []string{"install", "--no-audit", "--no-fund", "--ignore-scripts", "--omit=optional", "--save-exact", "--loglevel=error", spec.InstallSpec},
		Env:                    bridgeSetupEnv(),
		Dir:                    dir,
		Stdout:                 &stdout,
		Stderr:                 &stderr,
		MaxCapturedOutputBytes: codeSandboxBridgeOutputLimit,
		CancelGracePeriod:      2 * time.Second,
	})
	if runErr != nil || result.ExitCode != 0 {
		return bridgeSetupError(result.ExitCode, stdout.String(), stderr.String(), runErr)
	}
	if !bridgeSDKPackageInstalled(dir, spec) {
		return fmt.Errorf("codesandbox bridge SDK setup did not install expected package %s", spec.InstallSpec)
	}
	if err := os.WriteFile(bridgeSDKMarkerPath(dir), []byte(spec.InstallSpec+"\n"+spec.ImportSpec+"\n"), 0o600); err != nil {
		return fmt.Errorf("write codesandbox bridge SDK marker: %w", err)
	}
	return nil
}

func bridgeOutputLimit(req BridgeRequest) int {
	if req.Operation == "run_command" {
		return codeSandboxRunCommandOutputLimit
	}
	return codeSandboxBridgeOutputLimit
}

func bridgeEnv(cfg CodeSandboxConfig, token, importSpec string) []string {
	env := make([]string, 0, 8)
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "SystemRoot", "SYSTEMROOT", "COMSPEC", "PATHEXT"} {
		if value := os.Getenv(key); value != "" {
			env = upsertEnv(env, key, value)
		}
	}
	env = upsertEnv(env, codesandboxFallbackAPIKeyEnv, token)
	env = upsertEnv(env, "CRABBOX_CODESANDBOX_SDK_PACKAGE", sdkPackage(cfg))
	return upsertEnv(env, "CRABBOX_CODESANDBOX_SDK_IMPORT", importSpec)
}

func bridgeSetupEnv() []string {
	env := make([]string, 0, 8)
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "SystemRoot", "SYSTEMROOT", "COMSPEC", "PATHEXT"} {
		if value := os.Getenv(key); value != "" {
			env = upsertEnv(env, key, value)
		}
	}
	return env
}

func bridgeWorkingDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(codeSandboxBridgeCacheDirEnv)); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("create codesandbox bridge working directory: %w", err)
		}
		return dir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "crabbox", "codesandbox-bridge")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create codesandbox bridge working directory: %w", err)
	}
	return dir, nil
}

type bridgeSDKSpec struct {
	InstallSpec     string
	ImportSpec      string
	ExpectedVersion string
	Install         bool
}

func bridgeSDKSpecFor(cfg CodeSandboxConfig) bridgeSDKSpec {
	installSpec := sdkPackage(cfg)
	importSpec, ok := npmPackageName(installSpec)
	if !ok {
		return bridgeSDKSpec{InstallSpec: installSpec, ImportSpec: installSpec}
	}
	version, _ := npmExactPackageVersion(installSpec, importSpec)
	return bridgeSDKSpec{InstallSpec: installSpec, ImportSpec: importSpec, ExpectedVersion: version, Install: true}
}

func npmPackageName(spec string) (string, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") ||
		strings.Contains(spec, ":") {
		return "", false
	}
	if strings.HasPrefix(spec, "@") {
		scopeEnd := strings.Index(spec, "/")
		if scopeEnd <= 1 || scopeEnd == len(spec)-1 {
			return "", false
		}
		rest := spec[scopeEnd+1:]
		nameEnd := len(rest)
		if idx := strings.Index(rest, "@"); idx >= 0 {
			nameEnd = idx
		}
		if slash := strings.Index(rest[:nameEnd], "/"); slash >= 0 {
			nameEnd = slash
		}
		if nameEnd == 0 {
			return "", false
		}
		return spec[:scopeEnd+1+nameEnd], true
	}
	nameEnd := len(spec)
	if idx := strings.Index(spec, "@"); idx >= 0 {
		nameEnd = idx
	}
	if slash := strings.Index(spec[:nameEnd], "/"); slash >= 0 {
		nameEnd = slash
	}
	if nameEnd == 0 {
		return "", false
	}
	return spec[:nameEnd], true
}

func npmExactPackageVersion(spec, packageName string) (string, bool) {
	suffix := strings.TrimPrefix(strings.TrimSpace(spec), packageName)
	if !strings.HasPrefix(suffix, "@") {
		return "", false
	}
	version := strings.TrimPrefix(suffix, "@")
	if version == "" || version[0] < '0' || version[0] > '9' ||
		strings.ContainsAny(version, " /\\*^~<>=|") {
		return "", false
	}
	return version, true
}

func bridgeSDKInstalled(dir string, spec bridgeSDKSpec) bool {
	marker, err := os.ReadFile(bridgeSDKMarkerPath(dir))
	if err != nil || string(marker) != spec.InstallSpec+"\n"+spec.ImportSpec+"\n" {
		return false
	}
	return bridgeSDKPackageInstalled(dir, spec)
}

func bridgeSDKPackageInstalled(dir string, spec bridgeSDKSpec) bool {
	packagePath := filepath.Join(dir, "node_modules", filepath.FromSlash(spec.ImportSpec), "package.json")
	data, err := os.ReadFile(packagePath)
	if err != nil {
		return false
	}
	if spec.ExpectedVersion == "" {
		return true
	}
	var metadata struct {
		Version string `json:"version"`
	}
	return json.Unmarshal(data, &metadata) == nil && metadata.Version == spec.ExpectedVersion
}

func writeBridgePackageJSON(dir string) error {
	const data = `{"private":true,"type":"module"}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(data), 0o600); err != nil {
		return fmt.Errorf("write codesandbox bridge package.json: %w", err)
	}
	return nil
}

func bridgeSDKMarkerPath(dir string) string {
	return filepath.Join(dir, ".crabbox-codesandbox-sdk")
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

func bridgeSetupError(exitCode int, stdout, stderr string, err error) error {
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
	if err != nil {
		return fmt.Errorf("codesandbox bridge SDK setup failed exit=%d: %s: %w", exitCode, message, err)
	}
	return fmt.Errorf("codesandbox bridge SDK setup failed exit=%d: %s", exitCode, message)
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
  process.stdout.write(JSON.stringify(value), () => process.exit(0));
}
function fail(code, message) {
  emit({ ok: false, error: { code, message: String(message || "") } });
}
function normalizeSandbox(sandbox, state = "") {
  if (!sandbox) return {};
  const tags = Array.isArray(sandbox.tags) ? sandbox.tags : [];
  return {
    id: sandbox.id || sandbox.sandboxId || sandbox.uid || "",
    title: sandbox.title || sandbox.name || "",
    privacy: sandbox.privacy || "",
    tags,
    state,
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
  return await callAny(sandboxes, ["get"], id);
}
async function resumeSandbox(sdk, id) {
  const sandboxes = sdk.sandboxes || sdk;
  return await callAny(sandboxes, ["resume"], id);
}
async function connectSandbox(sdk, id) {
  const sandbox = await resumeSandbox(sdk, id);
  if (sandbox && typeof sandbox.connect === "function") {
    return { sandbox, client: await sandbox.connect() };
  }
  return { sandbox, client: sandbox };
}
async function createSandbox(sdk, VMTier) {
  const sandboxes = sdk.sandboxes || sdk;
  const options = {};
  if (req.title) options.title = req.title;
  if (Array.isArray(req.tags) && req.tags.length) options.tags = req.tags;
  if (req.templateId) options.id = req.templateId;
  if (req.privacy) options.privacy = req.privacy;
  if (req.vmTier) options.vmTier = resolveVMTier(VMTier, req.vmTier);
  if (req.hibernationTimeoutSecs) options.hibernationTimeoutSeconds = Number(req.hibernationTimeoutSecs);
  options.automaticWakeupConfig = {
    http: !!req.automaticWakeupHttp,
    websocket: !!req.automaticWakeupWebSocket
  };
  return await callAny(sandboxes, ["create", "createSandbox"], options);
}
function resolveVMTier(VMTier, value) {
  if (!VMTier || !Array.isArray(VMTier.All)) {
    throw new Error("CodeSandbox SDK does not expose VMTier.All");
  }
  const normalized = String(value || "").trim().toLowerCase();
  const tier = VMTier.All.find((candidate) => String(candidate && candidate.name || "").toLowerCase() === normalized);
  if (!tier) throw new Error("unsupported CodeSandbox VM tier: " + String(value || ""));
  return tier;
}
async function runningSandboxIDs(sdk) {
  try {
    const running = await callAny(sdk.sandboxes || sdk, ["listRunning"]);
    const vms = running && Array.isArray(running.vms) ? running.vms : [];
    return new Set(vms.map((vm) => String(vm && vm.id || "")).filter(Boolean));
  } catch {
    // listRunning is a delayed snapshot; metadata operations must not depend on it.
    return new Set();
  }
}
function shellQuote(value) {
  const text = String(value ?? "");
  if (text === "") return "''";
  if (/^[A-Za-z0-9_@%+=:,./-]+$/.test(text)) return text;
  return "'" + text.replace(/'/g, "'\\''") + "'";
}
function validEnvEntries(env) {
  return Object.entries(env || {})
    .filter(([key]) => /^[A-Za-z_][A-Za-z0-9_]*$/.test(key));
}
function envFileContent(env) {
  return validEnvEntries(env)
    .map(([key, value]) => "export " + key + "=" + shellQuote(value))
    .join("\n") + "\n";
}
function commandLineFromRequest(command, cwd, envFilePath) {
  const parts = ["cd", shellQuote(cwd), "&&"];
  if (envFilePath) {
    const quoted = shellQuote(envFilePath);
    parts.push("(chmod", "600", quoted, "2>/dev/null", "||", "true)", "&&", "set", "-a", "&&", ".", quoted, "&&", "set", "+a", "&&", "rm", "-f", quoted, "&&");
  }
  parts.push("exec", ...command.map(shellQuote));
  return parts.join(" ");
}
function workspaceFilePath(path) {
  const value = String(path || "");
  if (value === "/project/workspace") return ".";
  if (value.startsWith("/project/workspace/")) return value.slice("/project/workspace/".length);
  if (value.startsWith("/")) {
    throw new Error("CodeSandbox SDK file path must be under /project/workspace");
  }
  return value;
}
async function runCommand(sandbox) {
  const command = Array.isArray(req.command) ? req.command.map((v) => String(v ?? "")) : [];
  if (!command.length || command[0] === "") throw new Error("missing command");
  const cwd = req.cwd || "/project/workspace";
  const env = req.env || {};
  const commands = sandbox.commands || sandbox.command || sandbox;
  let envFilePath = "";
  if (validEnvEntries(env).length) {
    envFilePath = "/project/workspace/.crabbox-env-" + process.pid + "-" + Date.now() + "-" + Math.random().toString(16).slice(2) + ".sh";
    await writeWorkspaceBuffer(sandbox, envFilePath, Buffer.from(envFileContent(env), "utf8"));
  }
  const commandLine = commandLineFromRequest(command, cwd, envFilePath);
  let result;
  if (commands && typeof commands.run === "function") {
    try {
      result = await commands.run(commandLine);
    } catch (err) {
      if (err && Number.isInteger(err.exitCode)) {
        return { exitCode: err.exitCode, stdout: String(err.output || ""), stderr: "" };
      }
      throw err;
    }
  } else {
    result = await callAny(commands, ["exec", "runCommand"], {
      command: commandLine,
      cmd: commandLine
    });
  }
  if (typeof result === "string") {
    return { exitCode: 0, stdout: result, stderr: "" };
  }
  return {
    exitCode: Number(result && (result.exitCode ?? result.code ?? result.status) || 0),
    stdout: String(result && (result.stdout ?? result.output ?? "") || ""),
    stderr: String(result && (result.stderr ?? result.errorOutput ?? "") || "")
  };
}
async function writeWorkspaceBuffer(sandbox, path, buffer) {
  const files = sandbox.filesystem || sandbox.fs || sandbox.files || sandbox;
  const targetPath = workspaceFilePath(path);
  if (files && typeof files.writeFile === "function") {
    await files.writeFile(targetPath, buffer);
    return;
  }
  if (files && typeof files.write === "function") {
    await files.write(targetPath, buffer);
    return;
  }
  await callAny(files, ["writeTextFile", "createFile"], targetPath, buffer);
}
async function writeFile(sandbox) {
  const buffer = Buffer.from(String(req.contentBase64 || ""), "base64");
  await writeWorkspaceBuffer(sandbox, req.path, buffer);
}
function normalizePort(portInfo, fallbackPort, fallbackURL) {
  const port = Number((portInfo && (portInfo.port ?? portInfo.targetPort ?? portInfo.containerPort)) || fallbackPort || 0);
  const rawHost = String(fallbackURL || (portInfo && (portInfo.host ?? portInfo.url ?? portInfo.href)) || "").trim();
  const host = rawHost && !/^[a-z][a-z0-9+.-]*:\/\//i.test(rawHost)
    ? (rawHost.startsWith("//") ? "https:" + rawHost : "https://" + rawHost)
    : rawHost;
  return { port, host, url: host };
}
async function getHostURL(sdk, client, sandboxID, port) {
  if (sdk.hosts && typeof sdk.hosts.createToken === "function" && typeof sdk.hosts.getUrl === "function") {
    const hostToken = await sdk.hosts.createToken(sandboxID, {
      expiresAt: new Date(Date.now() + 60 * 60 * 1000)
    });
    return String(sdk.hosts.getUrl(hostToken, port) || "");
  }
  if (client && client.hosts && typeof client.hosts.getUrl === "function") {
    return String(client.hosts.getUrl(port) || "");
  }
  throw new Error("CodeSandbox SDK does not expose hosts.getUrl");
}
async function listPorts(sdk) {
  const { client } = await connectSandbox(sdk, req.sandboxId);
  const ports = client && client.ports;
  if (!ports || typeof ports.getAll !== "function") {
    throw new Error("CodeSandbox SDK does not expose ports.getAll");
  }
  return Array.from(await ports.getAll()).map((portInfo) => normalizePort(portInfo));
}
async function waitForPortURL(sdk) {
  const port = Number(req.port || 0);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error("port must be an integer between 1 and 65535");
  }
  const { client } = await connectSandbox(sdk, req.sandboxId);
  const ports = client && client.ports;
  let portInfo = {};
  if (ports && typeof ports.waitForPort === "function") {
    portInfo = await ports.waitForPort(port);
  } else {
    throw new Error("CodeSandbox SDK does not expose ports.waitForPort");
  }
  const url = await getHostURL(sdk, client, req.sandboxId, port);
  return normalizePort(portInfo, port, url);
}
try {
  const token = process.env.CSB_API_KEY || process.env.CRABBOX_CODESANDBOX_API_KEY || "";
  if (!token) {
    fail("auth_missing", "missing CodeSandbox API key");
  } else {
    const pkg = process.env.CRABBOX_CODESANDBOX_SDK_IMPORT || process.env.CRABBOX_CODESANDBOX_SDK_PACKAGE || "@codesandbox/sdk";
    const sdkModule = await import(pkg);
    const { CodeSandbox, VMTier } = sdkModule;
    const sdk = new CodeSandbox(token);
    if (req.operation === "list_sandboxes") {
      const listed = await callAny(sdk.sandboxes || sdk, ["list", "listSandboxes"], { limit: Number(req.limit || 1) });
      const items = listed.sandboxes || listed.items || listed.results || listed || [];
      const runningIDs = await runningSandboxIDs(sdk);
      const sandboxes = Array.from(items).map((sandbox) => normalizeSandbox(sandbox, runningIDs.has(String(sandbox && sandbox.id || "")) ? "running" : ""));
      emit({ ok: true, sandboxes, totalCount: listed.totalCount || listed.total || sandboxes.length });
    } else if (req.operation === "create_sandbox") {
      const sandbox = await createSandbox(sdk, VMTier);
      emit({ ok: true, sandbox: normalizeSandbox(sandbox, "running") });
    } else if (req.operation === "get_sandbox") {
      const sandbox = await openSandbox(sdk, req.sandboxId);
      const runningIDs = await runningSandboxIDs(sdk);
      emit({ ok: true, sandbox: normalizeSandbox(sandbox, runningIDs.has(String(req.sandboxId)) ? "running" : "") });
    } else if (req.operation === "delete_sandbox") {
      await callAny(sdk.sandboxes || sdk, ["delete", "deleteSandbox"], req.sandboxId);
      emit({ ok: true });
    } else if (req.operation === "hibernate_sandbox") {
      await callAny(sdk.sandboxes || sdk, ["hibernate"], req.sandboxId);
      emit({ ok: true });
    } else if (req.operation === "resume_sandbox") {
      const sandbox = await resumeSandbox(sdk, req.sandboxId);
      emit({ ok: true, sandbox: normalizeSandbox(sandbox, "running") });
    } else if (req.operation === "run_command") {
      const { client } = await connectSandbox(sdk, req.sandboxId);
      const command = await runCommand(client);
      emit({ ok: true, command });
    } else if (req.operation === "write_file") {
      const { client } = await connectSandbox(sdk, req.sandboxId);
      await writeFile(client);
      emit({ ok: true });
    } else if (req.operation === "list_ports") {
      const ports = await listPorts(sdk);
      emit({ ok: true, ports });
    } else if (req.operation === "get_port_url") {
      const port = await waitForPortURL(sdk);
      emit({ ok: true, port });
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
