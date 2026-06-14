package codesandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestSDKBridgeSendsJSONOnStdinAndTokenOnlyInEnv(t *testing.T) {
	setBridgeTestCacheDir(t)
	secret := "csb-secret-value"
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret")
	runner := &recordingBridgeRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		for _, arg := range req.Args {
			if strings.Contains(arg, secret) {
				t.Fatalf("secret leaked into argv: %#v", req.Args)
			}
		}
		if !envContains(req.Env, codesandboxFallbackAPIKeyEnv+"="+secret) {
			t.Fatalf("bridge env missing SDK auth token")
		}
		if !envContains(req.Env, "CRABBOX_CODESANDBOX_SDK_PACKAGE=@codesandbox/sdk@2.4.2") {
			t.Fatalf("bridge env missing SDK package")
		}
		if !envContains(req.Env, "CRABBOX_CODESANDBOX_SDK_IMPORT=@codesandbox/sdk") {
			t.Fatalf("bridge env missing SDK import")
		}
		if envContains(req.Env, "AWS_SECRET_ACCESS_KEY=ambient-secret") {
			t.Fatalf("bridge env inherited unrelated ambient secret: %#v", req.Env)
		}
		if strings.TrimSpace(req.Dir) == "" {
			t.Fatal("bridge must run from a trusted working directory outside the repository cwd")
		}
		var payload BridgeRequest
		if err := json.Unmarshal([]byte(readRequestBody(req)), &payload); err != nil {
			t.Fatalf("stdin payload: %v", err)
		}
		if payload.Operation != "list_sandboxes" || payload.Limit != 2 {
			t.Fatalf("payload=%#v", payload)
		}
		_, _ = io.WriteString(req.Stdout, `{"ok":true,"sandboxes":[{"id":"csb_1","title":"my-app","privacy":"private","tags":["crabbox"]}],"totalCount":1}`)
		return LocalCommandResult{ExitCode: 0}, nil
	}}
	bridge := NewSDKBridge(newTestConfig().CodeSandbox, Runtime{Exec: runner})
	resp, err := bridge.RoundTrip(context.Background(), secret, BridgeRequest{Operation: "list_sandboxes", Limit: 2})
	if err != nil {
		t.Fatalf("RoundTrip err=%v", err)
	}
	if len(resp.Sandboxes) != 1 || resp.Sandboxes[0].ID != "csb_1" || resp.TotalCount != 1 {
		t.Fatalf("response=%#v", resp)
	}
	setup := runner.onlySetupCall(t)
	if !reflect.DeepEqual(setup.Args, []string{"install", "--no-audit", "--no-fund", "--ignore-scripts", "--omit=optional", "--save-exact", "--loglevel=error", "@codesandbox/sdk@2.4.2"}) {
		t.Fatalf("setup args=%#v", setup.Args)
	}
	if envContains(setup.Env, codesandboxFallbackAPIKeyEnv+"="+secret) ||
		envContains(setup.Env, "AWS_SECRET_ACCESS_KEY=ambient-secret") {
		t.Fatalf("setup env leaked secret material: %#v", setup.Env)
	}
	call := runner.onlyCall(t)
	if call.Name != "node" {
		t.Fatalf("command=%q", call.Name)
	}
	if !reflect.DeepEqual(call.Args[:2], []string{"--input-type=module", "-e"}) {
		t.Fatalf("args=%#v", call.Args)
	}
}

func TestSDKBridgeRunCommandUsesCommandTimeoutAndLargerCaptureLimit(t *testing.T) {
	setBridgeTestCacheDir(t)
	secret := "csb-secret-value"
	runner := &recordingBridgeRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		_, _ = io.WriteString(req.Stdout, `{"ok":true,"command":{"exitCode":0}}`)
		return LocalCommandResult{ExitCode: 0}, nil
	}}
	cfg := newTestConfig().CodeSandbox
	cfg.OperationTimeoutSecs = 30
	bridge := NewSDKBridge(cfg, Runtime{Exec: runner})
	if _, err := bridge.RoundTrip(context.Background(), secret, BridgeRequest{
		Operation: "run_command",
		SandboxID: "sb_1",
		Command:   []string{"sleep", "60"},
		Timeout:   3600,
	}); err != nil {
		t.Fatalf("RoundTrip err=%v", err)
	}
	call := runner.onlyCall(t)
	if call.MaxCapturedOutputBytes != codeSandboxRunCommandOutputLimit {
		t.Fatalf("run command capture limit=%d, want %d", call.MaxCapturedOutputBytes, codeSandboxRunCommandOutputLimit)
	}
	if len(runner.deadlines) != 2 {
		t.Fatalf("deadlines=%d want 2", len(runner.deadlines))
	}
	remaining := time.Until(runner.deadlines[1])
	if remaining < 3500*time.Second {
		t.Fatalf("run command bridge deadline too short: %s", remaining)
	}
}

func TestSDKBridgeRedactsTokenFromCommandFailures(t *testing.T) {
	setBridgeTestCacheDir(t)
	secret := "csb-secret-value"
	runner := &recordingBridgeRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		_, _ = io.WriteString(req.Stderr, "denied "+secret)
		return LocalCommandResult{ExitCode: 1}, errors.New("exit status 1")
	}}
	bridge := NewSDKBridge(newTestConfig().CodeSandbox, Runtime{Exec: runner})
	_, err := bridge.RoundTrip(context.Background(), secret, BridgeRequest{Operation: "list_sandboxes"})
	if err == nil {
		t.Fatal("expected bridge failure")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error was not redacted: %v", err)
	}
}

func TestSDKBridgeRedactsTokenFromBridgeErrorResponse(t *testing.T) {
	setBridgeTestCacheDir(t)
	secret := "csb-secret-value"
	runner := &recordingBridgeRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		_, _ = io.WriteString(req.Stdout, `{"ok":false,"error":{"code":"auth_denied","message":"bad `+secret+`"}}`)
		return LocalCommandResult{ExitCode: 0}, nil
	}}
	bridge := NewSDKBridge(newTestConfig().CodeSandbox, Runtime{Exec: runner})
	_, err := bridge.RoundTrip(context.Background(), secret, BridgeRequest{Operation: "list_sandboxes"})
	if err == nil {
		t.Fatal("expected bridge error response")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error was not redacted: %v", err)
	}
}

func TestSDKBridgeScriptAwaitsAsyncPortListing(t *testing.T) {
	if !strings.Contains(codeSandboxBridgeScript, "await ports.getAll()") {
		t.Fatalf("bridge script must await CodeSandbox ports.getAll() before Array.from")
	}
	if strings.Contains(codeSandboxBridgeScript, "Array.from(await ports.getAll()).find") {
		t.Fatalf("bridge script must not synthesize publish success from a one-shot ports.getAll() lookup")
	}
	if !strings.Contains(codeSandboxBridgeScript, "expiresAt: new Date") {
		t.Fatalf("bridge script must create CodeSandbox host tokens with an expiry")
	}
	if strings.Contains(codeSandboxBridgeScript, "req.command.filter") {
		t.Fatalf("bridge script must preserve empty command arguments after the executable")
	}
	if !strings.Contains(codeSandboxBridgeScript, "command.map((v) => String(v ?? \"\"))") {
		t.Fatalf("bridge script must normalize command arguments without dropping empty strings")
	}
	if !strings.Contains(codeSandboxBridgeScript, "const { client } = await connectSandbox(sdk, req.sandboxId);\n      const command = await runCommand(client);") {
		t.Fatalf("bridge script must run commands through the connected CodeSandbox client")
	}
	if !strings.Contains(codeSandboxBridgeScript, "const { client } = await connectSandbox(sdk, req.sandboxId);\n      await writeFile(client);") {
		t.Fatalf("bridge script must write files through the connected CodeSandbox client")
	}
	if strings.Contains(codeSandboxBridgeScript, "commands.run(command[0]") {
		t.Fatalf("bridge script must use the documented command string SDK signatures")
	}
	if !strings.Contains(codeSandboxBridgeScript, "process.stdout.write(JSON.stringify(value), () => process.exit(0));") {
		t.Fatalf("bridge script must exit after flushing its one-shot response")
	}
	if !strings.Contains(codeSandboxBridgeScript, "result = await commands.run(commandLine);") {
		t.Fatalf("bridge script must pass one command string to CodeSandbox commands.run")
	}
	if !strings.Contains(codeSandboxBridgeScript, "if (typeof result === \"string\")") ||
		!strings.Contains(codeSandboxBridgeScript, "return { exitCode: 0, stdout: result, stderr: \"\" };") {
		t.Fatalf("bridge script must preserve string output returned by CodeSandbox commands.run")
	}
	if strings.Contains(codeSandboxBridgeScript, "parts.push(\"env\", ...assignments)") {
		t.Fatalf("bridge script must not embed forwarded env values in command text")
	}
	if !strings.Contains(codeSandboxBridgeScript, "await writeWorkspaceBuffer(sandbox, envFilePath, Buffer.from(envFileContent(env), \"utf8\"))") ||
		!strings.Contains(codeSandboxBridgeScript, "rm\", \"-f\", quoted") ||
		!strings.Contains(codeSandboxBridgeScript, "parts.push(\"exec\", ...command.map(shellQuote))") {
		t.Fatalf("bridge script must pass forwarded env through a temporary remote env file before exec")
	}
	if !strings.Contains(codeSandboxBridgeScript, "options.id = req.templateId") ||
		!strings.Contains(codeSandboxBridgeScript, "options.hibernationTimeoutSeconds") ||
		!strings.Contains(codeSandboxBridgeScript, "options.automaticWakeupConfig") {
		t.Fatalf("bridge script must use documented CodeSandbox create option names")
	}
	if !strings.Contains(codeSandboxBridgeScript, "options.vmTier = resolveVMTier(VMTier, req.vmTier)") ||
		!strings.Contains(codeSandboxBridgeScript, "VMTier.All.find") {
		t.Fatalf("bridge script must pass a CodeSandbox VMTier object instead of a string")
	}
	if !strings.Contains(codeSandboxBridgeScript, "const runningIDs = await runningSandboxIDs(sdk)") ||
		!strings.Contains(codeSandboxBridgeScript, "[\"listRunning\"]") ||
		!strings.Contains(codeSandboxBridgeScript, "metadata operations must not depend on it") {
		t.Fatalf("bridge script must use listRunning only as best-effort positive state evidence")
	}
	if !strings.Contains(codeSandboxBridgeScript, "Number.isInteger(err.exitCode)") ||
		!strings.Contains(codeSandboxBridgeScript, "stdout: String(err.output || \"\")") {
		t.Fatalf("bridge script must preserve CodeSandbox CommandError exit code and output")
	}
	if strings.Contains(codeSandboxBridgeScript, "[\"get\", \"connect\", \"open\", \"resume\"]") {
		t.Fatalf("read-only openSandbox must not resume or connect hibernated sandboxes")
	}
	if !strings.Contains(codeSandboxBridgeScript, "return await callAny(sandboxes, [\"get\"], id);") {
		t.Fatalf("read-only openSandbox must use SDK get only")
	}
	if !strings.Contains(codeSandboxBridgeScript, "function workspaceFilePath(path)") ||
		!strings.Contains(codeSandboxBridgeScript, "value.slice(\"/project/workspace/\".length)") ||
		!strings.Contains(codeSandboxBridgeScript, "await files.writeFile(targetPath, buffer)") {
		t.Fatalf("bridge script must convert workspace absolute file paths before SDK writes")
	}
	if !strings.Contains(codeSandboxBridgeScript, "process.env.CRABBOX_CODESANDBOX_SDK_IMPORT") {
		t.Fatalf("bridge script must import the resolved SDK package name from the trusted cache package")
	}
}

func TestSDKBridgeExecutesAgainstDocumentedSDKContracts(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required")
	}
	modulePath := filepath.Join(t.TempDir(), "fake-codesandbox-sdk.mjs")
	const module = `
export class VMTier {}
VMTier.All = ["Pico", "Nano", "Micro", "Small", "Medium", "Large", "XLarge"].map((name) => ({ name }));
let listRunningCalls = 0;

export class CodeSandbox {
  constructor(token) {
    if (token !== "secret") throw new Error("bad token");
    this.sandboxes = {
      create: async (opts) => {
        if (!opts.vmTier || opts.vmTier.name !== "Micro") throw new Error("vmTier was not a VMTier object");
        return { id: "sb_created", title: "created", tags: ["crabbox"] };
      },
      get: async (id) => ({ id, title: id, tags: ["crabbox"] }),
      list: async () => ({ sandboxes: [{ id: "sb_running" }, { id: "sb_idle" }], totalCount: 2 }),
      listRunning: async () => {
        listRunningCalls++;
        if (listRunningCalls === 3) throw new Error("snapshot unavailable");
        return { concurrentVmCount: 1, concurrentVmLimit: 5, vms: [{ id: "sb_running" }] };
      },
      resume: async (id) => ({
        id,
        connect: async () => ({
          commands: {
            run: async () => {
              const err = new Error("command failed");
              err.exitCode = 7;
              err.output = "EXPECTED_FAILURE_OUTPUT";
              throw err;
            }
          },
          ports: {
            getAll: async () => [{ port: 3000, host: "sb_running-3000.csb.app" }],
            waitForPort: async (port) => ({ port })
          },
        })
      })
    };
    this.hosts = {
      createToken: async (sandboxId) => ({ sandboxId, token: "preview-secret" }),
      getUrl: (token, port) => "https://" + token.sandboxId + "-" + port + ".csb.app?preview_token=" + token.token
    };
  }
}
`
	if err := os.WriteFile(modulePath, []byte(module), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := newTestConfig().CodeSandbox
	cfg.SDKPackage = (&url.URL{Scheme: "file", Path: modulePath}).String()
	bridge := NewSDKBridge(cfg, Runtime{Exec: actualBridgeRunner{}})

	created, err := bridge.RoundTrip(context.Background(), "secret", BridgeRequest{Operation: "create_sandbox", VMTier: "micro"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Sandbox.ID != "sb_created" || created.Sandbox.State != "running" {
		t.Fatalf("created=%#v", created.Sandbox)
	}
	running, err := bridge.RoundTrip(context.Background(), "secret", BridgeRequest{Operation: "get_sandbox", SandboxID: "sb_running"})
	if err != nil {
		t.Fatalf("get running: %v", err)
	}
	idle, err := bridge.RoundTrip(context.Background(), "secret", BridgeRequest{Operation: "get_sandbox", SandboxID: "sb_idle"})
	if err != nil {
		t.Fatalf("get idle: %v", err)
	}
	unknown, err := bridge.RoundTrip(context.Background(), "secret", BridgeRequest{Operation: "get_sandbox", SandboxID: "sb_snapshot_error"})
	if err != nil {
		t.Fatalf("get with unavailable running snapshot: %v", err)
	}
	if running.Sandbox.State != "running" || idle.Sandbox.State != "" || unknown.Sandbox.State != "" {
		t.Fatalf("states running=%q idle=%q", running.Sandbox.State, idle.Sandbox.State)
	}
	command, err := bridge.RoundTrip(context.Background(), "secret", BridgeRequest{
		Operation: "run_command",
		SandboxID: "sb_running",
		Command:   []string{"sh", "-lc", "exit 7"},
	})
	if err != nil {
		t.Fatalf("run command: %v", err)
	}
	if command.Command.ExitCode != 7 || command.Command.Stdout != "EXPECTED_FAILURE_OUTPUT" {
		t.Fatalf("command=%#v", command.Command)
	}
	ports, err := bridge.RoundTrip(context.Background(), "secret", BridgeRequest{
		Operation: "list_ports",
		SandboxID: "sb_running",
	})
	if err != nil {
		t.Fatalf("list ports: %v", err)
	}
	if len(ports.Ports) != 1 || ports.Ports[0].Host != "https://sb_running-3000.csb.app" {
		t.Fatalf("ports=%#v", ports.Ports)
	}
	port, err := bridge.RoundTrip(context.Background(), "secret", BridgeRequest{
		Operation: "get_port_url",
		SandboxID: "sb_running",
		Port:      5173,
	})
	if err != nil {
		t.Fatalf("get port URL: %v", err)
	}
	if port.Port.Host != "https://sb_running-5173.csb.app?preview_token=preview-secret" || port.Port.URL != port.Port.Host {
		t.Fatalf("port=%#v", port.Port)
	}
}

func TestBridgeSDKInstalledRejectsWrongPinnedVersion(t *testing.T) {
	dir := t.TempDir()
	spec := bridgeSDKSpecFor(CodeSandboxConfig{SDKPackage: "@codesandbox/sdk@2.4.2"})
	packageDir := filepath.Join(dir, "node_modules", "@codesandbox", "sdk")
	if err := os.MkdirAll(packageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), []byte(`{"version":"2.4.1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bridgeSDKMarkerPath(dir), []byte(spec.InstallSpec+"\n"+spec.ImportSpec+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if bridgeSDKInstalled(dir, spec) {
		t.Fatal("wrong installed SDK version accepted")
	}
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), []byte(`{"version":"2.4.2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !bridgeSDKInstalled(dir, spec) {
		t.Fatal("matching installed SDK version rejected")
	}
}

func TestSDKBridgeClassifiesMalformedJSON(t *testing.T) {
	setBridgeTestCacheDir(t)
	runner := &recordingBridgeRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		_, _ = io.WriteString(req.Stdout, `not-json`)
		return LocalCommandResult{ExitCode: 0}, nil
	}}
	bridge := NewSDKBridge(newTestConfig().CodeSandbox, Runtime{Exec: runner})
	_, err := bridge.RoundTrip(context.Background(), "secret", BridgeRequest{Operation: "list_sandboxes"})
	if err == nil || !strings.Contains(err.Error(), "decode codesandbox bridge JSON") {
		t.Fatalf("RoundTrip err=%v", err)
	}
}

func TestCodeSandboxClientListsThroughBridge(t *testing.T) {
	setBridgeTestCacheDir(t)
	secret := "csb-secret-value"
	runner := &recordingBridgeRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		_, _ = io.WriteString(req.Stdout, `{"ok":true,"sandboxes":[{"id":"csb_1"}],"totalCount":7}`)
		return LocalCommandResult{ExitCode: 0}, nil
	}}
	client := &codeSandboxClient{
		cfg:    newTestConfig().CodeSandbox,
		rt:     Runtime{Exec: runner},
		bridge: NewSDKBridge(newTestConfig().CodeSandbox, Runtime{Exec: runner}),
		token:  secret,
	}
	result, err := client.ListSandboxes(context.Background(), ListSandboxesRequest{Limit: 3})
	if err != nil {
		t.Fatalf("ListSandboxes err=%v", err)
	}
	if result.TotalCount != 7 || len(result.Sandboxes) != 1 || result.Sandboxes[0].ID != "csb_1" {
		t.Fatalf("result=%#v", result)
	}
}

func TestCodeSandboxClientLifecycleOperationsUseBridgePayloads(t *testing.T) {
	setBridgeTestCacheDir(t)
	seen := []BridgeRequest{}
	runner := &recordingBridgeRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		var payload BridgeRequest
		if err := json.Unmarshal([]byte(readRequestBody(req)), &payload); err != nil {
			t.Fatalf("stdin payload: %v", err)
		}
		seen = append(seen, payload)
		switch payload.Operation {
		case "create_sandbox":
			_, _ = io.WriteString(req.Stdout, `{"ok":true,"sandbox":{"id":"sb_1","state":"running","tags":["crabbox"]}}`)
		case "get_sandbox":
			_, _ = io.WriteString(req.Stdout, `{"ok":true,"sandbox":{"id":"sb_1","state":"running"}}`)
		case "hibernate_sandbox":
			_, _ = io.WriteString(req.Stdout, `{"ok":true}`)
		case "resume_sandbox":
			_, _ = io.WriteString(req.Stdout, `{"ok":true,"sandbox":{"id":"sb_1","state":"running"}}`)
		case "run_command":
			_, _ = io.WriteString(req.Stdout, `{"ok":true,"command":{"exitCode":4,"stdout":"out\n","stderr":"err\n"}}`)
		case "write_file":
			if got, _ := base64.StdEncoding.DecodeString(payload.ContentBase64); string(got) != "archive-bytes" {
				t.Fatalf("upload content=%q", got)
			}
			_, _ = io.WriteString(req.Stdout, `{"ok":true}`)
		case "delete_sandbox":
			_, _ = io.WriteString(req.Stdout, `{"ok":true}`)
		case "list_ports":
			_, _ = io.WriteString(req.Stdout, `{"ok":true,"ports":[{"port":3000,"host":"https://sb_1-3000.csb.app"}]}`)
		case "get_port_url":
			_, _ = io.WriteString(req.Stdout, `{"ok":true,"port":{"port":5173,"host":"https://sb_1-5173.csb.app"}}`)
		default:
			t.Fatalf("unexpected operation %q", payload.Operation)
		}
		return LocalCommandResult{ExitCode: 0}, nil
	}}
	client := &codeSandboxClient{
		cfg:    newTestConfig().CodeSandbox,
		rt:     Runtime{Exec: runner},
		bridge: NewSDKBridge(newTestConfig().CodeSandbox, Runtime{Exec: runner}),
		token:  "secret",
	}

	if _, err := client.CreateSandbox(context.Background(), CreateSandboxRequest{Title: "crabbox-app", Tags: []string{"crabbox"}}); err != nil {
		t.Fatalf("CreateSandbox err=%v", err)
	}
	if _, err := client.GetSandbox(context.Background(), "sb_1"); err != nil {
		t.Fatalf("GetSandbox err=%v", err)
	}
	if err := client.HibernateSandbox(context.Background(), "sb_1"); err != nil {
		t.Fatalf("HibernateSandbox err=%v", err)
	}
	if _, err := client.ResumeSandbox(context.Background(), "sb_1"); err != nil {
		t.Fatalf("ResumeSandbox err=%v", err)
	}
	got, err := client.RunCommand(context.Background(), "sb_1", CommandRequest{
		Command: []string{"bash", "-lc", "echo ok"},
		Cwd:     "/project/workspace/app",
		Env:     map[string]string{"SECRET_TOKEN": "value"},
	})
	if err != nil {
		t.Fatalf("RunCommand err=%v", err)
	}
	if got.ExitCode != 4 || got.Stdout != "out\n" || got.Stderr != "err\n" {
		t.Fatalf("command result=%#v", got)
	}
	if err := client.UploadFile(context.Background(), "sb_1", "/tmp/archive.tgz", bytes.NewReader([]byte("archive-bytes"))); err != nil {
		t.Fatalf("UploadFile err=%v", err)
	}
	if err := client.DeleteSandbox(context.Background(), "sb_1"); err != nil {
		t.Fatalf("DeleteSandbox err=%v", err)
	}
	ports, err := client.ListPorts(context.Background(), "sb_1")
	if err != nil {
		t.Fatalf("ListPorts err=%v", err)
	}
	if len(ports) != 1 || ports[0].Port != 3000 {
		t.Fatalf("ports=%#v", ports)
	}
	port, err := client.WaitForPortURL(context.Background(), "sb_1", 5173)
	if err != nil {
		t.Fatalf("WaitForPortURL err=%v", err)
	}
	if port.Host != "https://sb_1-5173.csb.app" {
		t.Fatalf("port=%#v", port)
	}
	ops := make([]string, 0, len(seen))
	for _, req := range seen {
		ops = append(ops, req.Operation)
	}
	wantOps := []string{"create_sandbox", "get_sandbox", "hibernate_sandbox", "resume_sandbox", "run_command", "write_file", "delete_sandbox", "list_ports", "get_port_url"}
	if !reflect.DeepEqual(ops, wantOps) {
		t.Fatalf("ops=%v want %v", ops, wantOps)
	}
	if seen[4].Env["SECRET_TOKEN"] != "value" || seen[4].Cwd != "/project/workspace/app" {
		t.Fatalf("run payload=%#v", seen[4])
	}
	if seen[8].Port != 5173 {
		t.Fatalf("port payload=%#v", seen[8])
	}
}

type recordingBridgeRunner struct {
	calls     []LocalCommandRequest
	deadlines []time.Time
	fn        func(LocalCommandRequest) (LocalCommandResult, error)
}

func (r *recordingBridgeRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if deadline, ok := ctx.Deadline(); ok {
		r.deadlines = append(r.deadlines, deadline)
	}
	if req.Name == "npm" {
		spec := req.Args[len(req.Args)-1]
		name, ok := npmPackageName(spec)
		if !ok {
			return LocalCommandResult{ExitCode: 1}, errors.New("invalid package spec")
		}
		version, _ := npmExactPackageVersion(spec, name)
		packageDir := filepath.Join(req.Dir, "node_modules", filepath.FromSlash(name))
		if err := os.MkdirAll(packageDir, 0o700); err != nil {
			return LocalCommandResult{ExitCode: 1}, err
		}
		if err := os.WriteFile(filepath.Join(packageDir, "package.json"), []byte(`{"version":"`+version+`"}`), 0o600); err != nil {
			return LocalCommandResult{ExitCode: 1}, err
		}
		return LocalCommandResult{ExitCode: 0}, nil
	}
	if r.fn != nil {
		return r.fn(req)
	}
	return LocalCommandResult{ExitCode: 0}, nil
}

type actualBridgeRunner struct{}

func (actualBridgeRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	cmd.Dir = req.Dir
	cmd.Env = req.Env
	cmd.Stdin = req.Stdin
	cmd.Stdout = req.Stdout
	cmd.Stderr = req.Stderr
	err := cmd.Run()
	result := LocalCommandResult{}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	return result, err
}

func (r *recordingBridgeRunner) onlyCall(t *testing.T) LocalCommandRequest {
	t.Helper()
	calls := r.bridgeCalls()
	if len(calls) != 1 {
		t.Fatalf("bridge calls=%d want 1 (all calls=%d)", len(calls), len(r.calls))
	}
	return calls[0]
}

func (r *recordingBridgeRunner) onlySetupCall(t *testing.T) LocalCommandRequest {
	t.Helper()
	calls := r.setupCalls()
	if len(calls) != 1 {
		t.Fatalf("setup calls=%d want 1 (all calls=%d)", len(calls), len(r.calls))
	}
	return calls[0]
}

func (r *recordingBridgeRunner) bridgeCalls() []LocalCommandRequest {
	var calls []LocalCommandRequest
	for _, call := range r.calls {
		if call.Stdin != nil {
			calls = append(calls, call)
		}
	}
	return calls
}

func (r *recordingBridgeRunner) setupCalls() []LocalCommandRequest {
	var calls []LocalCommandRequest
	for _, call := range r.calls {
		if call.Name == "npm" {
			calls = append(calls, call)
		}
	}
	return calls
}

func readRequestBody(req LocalCommandRequest) string {
	if req.Stdin == nil {
		return ""
	}
	data, _ := io.ReadAll(req.Stdin)
	return string(data)
}

func envContains(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func setBridgeTestCacheDir(t *testing.T) {
	t.Helper()
	t.Setenv(codeSandboxBridgeCacheDirEnv, t.TempDir())
}

var _ core.CommandRunner = (*recordingBridgeRunner)(nil)
