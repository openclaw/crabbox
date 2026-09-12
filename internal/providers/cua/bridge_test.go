package cua

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	calls []core.LocalCommandRequest
	fn    func(core.LocalCommandRequest) (core.LocalCommandResult, error)
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.fn != nil {
		var stdout, stderr bytes.Buffer
		req.Stdout, req.Stderr = &stdout, &stderr
		result, err := r.fn(req)
		if result.Stdout == "" {
			result.Stdout = stdout.String()
		}
		if result.Stderr == "" {
			result.Stderr = stderr.String()
		}
		return result, err
	}
	return core.LocalCommandResult{ExitCode: 0}, nil
}

func (r *recordingRunner) onlyCall(t *testing.T) core.LocalCommandRequest {
	t.Helper()
	if len(r.calls) != 1 {
		t.Fatalf("calls=%d want 1", len(r.calls))
	}
	return r.calls[0]
}

func testConfig() core.Config {
	return core.Config{Provider: providerName, Cua: core.CuaConfig{
		APIURL:            "https://api.cua.example/v1/",
		Image:             core.CuaConfigDefaultImage,
		Kind:              core.CuaConfigDefaultKind,
		Workdir:           core.CuaConfigDefaultWorkdir,
		ExecTimeoutSecs:   60,
		BridgeCommand:     core.CuaConfigDefaultBridgeCommand,
		SDKPackage:        core.CuaConfigDefaultSDKPackage,
		SDKImport:         core.CuaConfigDefaultSDKImport,
		SDKFallbackImport: core.CuaConfigDefaultSDKFallbackImport,
	}}
}

func requestBody(t *testing.T, req core.LocalCommandRequest) string {
	t.Helper()
	data, err := io.ReadAll(req.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func envContains(env []string, value string) bool {
	for _, item := range env {
		if item == value {
			return true
		}
	}
	return false
}

func TestBridgeSendsJSONOnStdinAndMapsSecretOnlyToSDKEnv(t *testing.T) {
	secret := "placeholder"
	t.Setenv("CRABBOX_CUA_API_KEY", secret)
	t.Setenv("CUA_TELEMETRY_ENABLED", "true")
	t.Setenv("HTTPS_PROXY", "https://proxy.example.test:8443")
	t.Setenv("NO_PROXY", "localhost,127.0.0.1")
	t.Setenv("SSL_CERT_FILE", "/etc/example-ca.pem")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "placeholder")
	runner := &recordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		for _, arg := range req.Args {
			if strings.Contains(arg, secret) {
				t.Fatalf("secret leaked into argv: %#v", req.Args)
			}
		}
		if !envContains(req.Env, "CUA_API_KEY="+secret) {
			t.Fatalf("bridge env missing SDK API key: %#v", req.Env)
		}
		if !envContains(req.Env, "CUA_TELEMETRY_ENABLED=false") {
			t.Fatalf("bridge must disable CUA SDK telemetry: %#v", req.Env)
		}
		for _, value := range []string{
			"HTTPS_PROXY=https://proxy.example.test:8443",
			"NO_PROXY=localhost,127.0.0.1",
			"SSL_CERT_FILE=/etc/example-ca.pem",
		} {
			if !envContains(req.Env, value) {
				t.Fatalf("bridge env missing connectivity setting %q: %#v", value, req.Env)
			}
		}
		if !envContains(req.Env, "CUA_BASE_URL=https://api.cua.example") || envContains(req.Env, "CUA_API_BASE=https://api.cua.example") {
			t.Fatalf("bridge env does not match pinned cua-sandbox v0.1.17 base URL contract: %#v", req.Env)
		}
		if envContains(req.Env, "AWS_SECRET_ACCESS_KEY=placeholder") {
			t.Fatalf("bridge env inherited unrelated ambient secret: %#v", req.Env)
		}
		if strings.TrimSpace(req.Dir) == "" || !strings.Contains(req.Dir, "cua-bridge") {
			t.Fatalf("bridge must run from a trusted cache directory, got %q", req.Dir)
		}
		info, err := os.Stat(req.Dir)
		if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
			t.Fatalf("bridge directory must be a private temporary directory: mode=%v err=%v", info, err)
		}
		if !envContains(req.Env, "HOME="+req.Dir) || !envContains(req.Env, "USERPROFILE="+req.Dir) {
			t.Fatalf("bridge must isolate SDK credential discovery from the user home: %#v", req.Env)
		}
		var payload bridgeRequest
		if err := json.Unmarshal([]byte(requestBody(t, req)), &payload); err != nil {
			t.Fatalf("stdin payload: %v", err)
		}
		if payload.Action != "doctor" || payload.Version != bridgeVersion || payload.Config.APIURL != "https://api.cua.example" || payload.Config.ExecTimeout != 60 {
			t.Fatalf("payload=%#v", payload)
		}
		_, _ = io.WriteString(req.Stdout, `{"ok":true,"doctor":{"importPath":"cua","auth":"env","checks":[{"status":"ok","check":"sdk"}]}}`)
		return core.LocalCommandResult{ExitCode: 0}, nil
	}}
	client := newBridgeClient(testConfig(), core.Runtime{Exec: runner})
	resp, err := client.RoundTrip(context.Background(), bridgeRequest{Action: "doctor"})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if !resp.OK || resp.Doctor.ImportPath != "cua" {
		t.Fatalf("resp=%#v", resp)
	}
	call := runner.onlyCall(t)
	if call.Name != core.CuaConfigDefaultBridgeCommand {
		t.Fatalf("command=%q", call.Name)
	}
	if !reflect.DeepEqual(call.Args[:2], []string{"-I", "-c"}) {
		t.Fatalf("args=%#v", call.Args)
	}
	if !strings.Contains(call.Args[2], "def doctor") {
		t.Fatalf("embedded script missing doctor implementation")
	}
	if call.MaxCapturedOutputBytes != bridgeOutputLimit || call.CancelGracePeriod != 2*time.Second {
		t.Fatalf("bridge limits=%d/%s", call.MaxCapturedOutputBytes, call.CancelGracePeriod)
	}
}

func TestNotFoundClassificationRequiresStructuredSignal(t *testing.T) {
	for _, err := range []error{
		&bridgeActionError{class: "not_found", code: "HTTPStatusError", msg: "missing"},
		&bridgeActionError{class: "cleanup_failed", code: "SandboxNotFoundError", msg: "missing"},
	} {
		if !isCUANotFound(err) {
			t.Fatalf("isCUANotFound(%v)=false, want true", err)
		}
	}
	for _, err := range []error{
		&bridgeActionError{class: "cleanup_failed", code: "RuntimeError", msg: "request to sandbox-404 timed out"},
		&bridgeActionError{class: "cleanup_failed", code: "RuntimeError", msg: "not found in response text"},
		&bridgeActionError{class: "cleanup_failed", code: "FileNotFoundError", msg: "local file missing"},
		&bridgeActionError{class: "cleanup_failed", code: "ModuleNotFoundError", msg: "SDK module missing"},
	} {
		if isCUANotFound(err) {
			t.Fatalf("isCUANotFound(%v)=true for unstructured message", err)
		}
	}
}

func TestBridgeRedactsSecretFromCommandFailure(t *testing.T) {
	secret := "placeholder"
	t.Setenv("CRABBOX_CUA_API_KEY", secret)
	proxy := "https://proxy.example.test:8443"
	t.Setenv("HTTPS_PROXY", proxy)
	runner := &recordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		_, _ = io.WriteString(req.Stderr, "denied "+secret+" via "+proxy)
		return core.LocalCommandResult{ExitCode: 1}, errors.New("exit status 1")
	}}
	client := newBridgeClient(testConfig(), core.Runtime{Exec: runner})
	_, err := client.RoundTrip(context.Background(), bridgeRequest{Action: "doctor"})
	if err == nil {
		t.Fatal("expected bridge failure")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), proxy) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error was not redacted: %v", err)
	}
}

func TestBridgeRedactsSecretBeforeTruncatingFailure(t *testing.T) {
	secret := strings.Repeat("s", 40)
	t.Setenv("CRABBOX_CUA_API_KEY", secret)
	runner := &recordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		_, _ = io.WriteString(req.Stderr, strings.Repeat("x", 500)+secret)
		return core.LocalCommandResult{ExitCode: 1}, errors.New("exit status 1")
	}}
	client := newBridgeClient(testConfig(), core.Runtime{Exec: runner})
	_, err := client.RoundTrip(context.Background(), bridgeRequest{Action: "doctor"})
	if err == nil {
		t.Fatal("expected bridge failure")
	}
	if strings.Contains(err.Error(), strings.Repeat("s", 12)) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("boundary-straddling secret was not redacted: %v", err)
	}
}

func TestBridgeMutationsFailClosedBeforeRunner(t *testing.T) {
	runner := &recordingRunner{}
	client := newBridgeClient(testConfig(), core.Runtime{Exec: runner})
	if _, err := client.CreateSandbox(context.Background(), map[string]string{"lease": "test"}); err == nil || !strings.Contains(err.Error(), "idempotency key") || !strings.Contains(err.Error(), cuaTrackingIssue) {
		t.Fatalf("CreateSandbox err=%v, want actionable provisioning guard", err)
	}
	if _, err := client.RoundTrip(context.Background(), bridgeRequest{Action: "create"}); err == nil || !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("RoundTrip(create) err=%v, want provisioning guard", err)
	}
	if _, err := client.RoundTrip(context.Background(), bridgeRequest{Action: "delete"}); err == nil || !strings.Contains(err.Error(), "atomically") {
		t.Fatalf("RoundTrip(delete) err=%v, want deletion guard", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("mutation guard must fail before Python bridge, calls=%d", len(runner.calls))
	}
}

func TestBridgeRequiresRunner(t *testing.T) {
	var exitErr core.ExitError
	_, err := newBridgeClient(testConfig(), core.Runtime{}).RoundTrip(context.Background(), bridgeRequest{Action: "doctor"})
	if !errors.As(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(err.Error(), "requires Runtime.Exec") {
		t.Fatalf("RoundTrip error=%v", err)
	}
}

func TestCUABridgeDefaultValuesAndRawConfig(t *testing.T) {
	defaults := core.BaseConfig().Cua
	if defaults.Image != "ubuntu:24.04" || defaults.Kind != "container" || defaults.Workdir != "/workspace/crabbox" || defaults.BridgeCommand != "python3" || defaults.SDKPackage != "cua" || defaults.SDKImport != "cua" || defaults.SDKFallbackImport != "cua_sandbox" || defaults.ExecTimeoutSecs != 600 {
		t.Fatalf("generated defaults changed: %#v", defaults)
	}
	for _, raw := range []string{"", " \t", "custom"} {
		cfg := testConfig()
		cfg.Cua.Image, cfg.Cua.Kind, cfg.Cua.SDKPackage, cfg.Cua.SDKImport = raw, raw, raw, raw
		cfg.Cua.ExecTimeoutSecs = 0
		before := cfg.Cua
		got := bridgeConfigForConfig(cfg)
		wantImage, wantKind, wantPackage, wantImport := "ubuntu:24.04", "container", "cua", "cua"
		if raw != "" {
			wantImage, wantKind, wantPackage, wantImport = strings.TrimSpace(raw), strings.TrimSpace(raw), strings.TrimSpace(raw), strings.TrimSpace(raw)
		}
		if got.Image != wantImage || got.Kind != wantKind || got.SDKPackage != wantPackage || got.SDKImport != wantImport || got.ExecTimeout != 0 {
			t.Fatalf("raw=%q bridge config=%#v", raw, got)
		}
		if cfg.Cua != before {
			t.Fatal("bridge config changed raw input")
		}
	}
	for _, seconds := range []int{-1, 0, 60} {
		cfg := testConfig()
		cfg.Cua.ExecTimeoutSecs = seconds
		want := 600 * time.Second
		if seconds > 0 {
			want = time.Duration(seconds) * time.Second
		}
		if bridgeTimeout(cfg, bridgeRequest{Action: "list"}) != want || bridgeTimeout(cfg, bridgeRequest{Action: "doctor"}) != 15*time.Second {
			t.Fatalf("unexpected budget for %d", seconds)
		}
	}
}

func TestCUABridgeLauncherAndWorkdirNormalization(t *testing.T) {
	t.Setenv("CRABBOX_CUA_API_KEY", "")
	t.Setenv("CUA_API_KEY", "")
	for _, tc := range []struct{ name, command, workdir, kind, wantCommand, wantWorkdir, wantKind string }{
		{"empty", "", "", "", "python3", "/workspace/crabbox", "container"},
		{"whitespace", " \t", " \t", " \t", "", "", ""},
		{"custom", " /opt/example-python ", " /workspace/app/ ", " VM ", "/opt/example-python", "/workspace/app", "vm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Cua.BridgeCommand, cfg.Cua.Workdir, cfg.Cua.Kind = tc.command, tc.workdir, tc.kind
			before := cfg.Cua
			runner := &recordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				var payload bridgeRequest
				if err := json.Unmarshal([]byte(requestBody(t, req)), &payload); err != nil {
					t.Fatal(err)
				}
				if req.Name != tc.wantCommand || payload.Config.Workdir != tc.wantWorkdir || payload.Config.Kind != tc.wantKind {
					t.Fatalf("command=%q workdir=%q kind=%q", req.Name, payload.Config.Workdir, payload.Config.Kind)
				}
				_, _ = io.WriteString(req.Stdout, `{"ok":true}`)
				return core.LocalCommandResult{ExitCode: 0}, nil
			}}
			if _, err := newBridgeClient(cfg, core.Runtime{Exec: runner}).RoundTrip(context.Background(), bridgeRequest{Action: "list"}); err != nil {
				t.Fatal(err)
			}
			_, err := cuaWorkdir(cfg)
			if (err != nil) != (tc.name == "whitespace") {
				t.Fatalf("workdir error=%v", err)
			}
			if cfg.Cua != before {
				t.Fatal("bridge changed raw config")
			}
		})
	}
}

func TestCUAEffectiveImportSelectionAcrossBridge(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	t.Setenv("CRABBOX_CUA_API_KEY", "")
	t.Setenv("CUA_API_KEY", "")
	for _, tc := range []struct{ name, preferred, fallback, wantPreferred, wantFallback string }{
		{"default", "cua", "", "cua", "cua_sandbox"},
		{"whitespace fallback", "cua", " \t", "cua", "cua_sandbox"},
		{"custom", " example_sdk ", " example_fallback ", "example_sdk", "example_fallback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Cua.SDKImport, cfg.Cua.SDKFallbackImport = tc.preferred, tc.fallback
			if err := validateProviderConfig(cfg); err != nil {
				t.Fatal(err)
			}
			before := cfg.Cua
			runner := &recordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				var payload bridgeRequest
				if err := json.Unmarshal([]byte(requestBody(t, req)), &payload); err != nil {
					t.Fatal(err)
				}
				selection := cuaScriptImportSelection(t, python, req.Args[2], payload, req.Env)
				if selection[0] != tc.wantPreferred || selection[1] != tc.wantFallback {
					t.Fatalf("effective imports=%v, want [%s %s]", selection, tc.wantPreferred, tc.wantFallback)
				}
				_, _ = io.WriteString(req.Stdout, `{"ok":true}`)
				return core.LocalCommandResult{ExitCode: 0}, nil
			}}
			if _, err := newBridgeClient(cfg, core.Runtime{Exec: runner}).RoundTrip(context.Background(), bridgeRequest{Action: "list"}); err != nil {
				t.Fatal(err)
			}
			if cfg.Cua != before {
				t.Fatal("bridge changed raw config")
			}
		})
	}
	for _, fromRequest := range []bool{true, false} {
		payload := bridgeRequest{Config: bridgeConfig{SDKImport: "request_sdk", FallbackImport: "request_fallback"}}
		want := []string{"request_sdk", "request_fallback"}
		if !fromRequest {
			payload.Config.SDKImport, payload.Config.FallbackImport = "", ""
			want = []string{"env_sdk", "env_fallback"}
		}
		got := cuaScriptImportSelection(t, python, bridgeScript, payload, []string{"CRABBOX_CUA_SDK_IMPORT=env_sdk", "CRABBOX_CUA_SDK_FALLBACK_IMPORT=env_fallback"})
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("request precedence=%t got=%v want=%v", fromRequest, got, want)
		}
	}
}

func TestCUAFallbackImportResolvedBeforeTransport(t *testing.T) {
	t.Setenv("CRABBOX_CUA_API_KEY", "")
	t.Setenv("CUA_API_KEY", "")
	for _, raw := range []string{"", " \t", " example_fallback "} {
		cfg := testConfig()
		cfg.Cua.SDKFallbackImport = raw
		want := "cua_sandbox"
		if strings.TrimSpace(raw) != "" {
			want = "example_fallback"
		}
		if got := bridgeConfigForConfig(cfg).FallbackImport; got != want {
			t.Fatalf("fallback request=%q, want %q", got, want)
		}
		if !envContains(bridgeEnv(cfg, t.TempDir()), "CRABBOX_CUA_SDK_FALLBACK_IMPORT="+want) {
			t.Fatal("fallback environment differs from request")
		}
		if cfg.Cua.SDKFallbackImport != raw {
			t.Fatal("raw fallback config changed")
		}
	}
}

// Stop at import selection: this fixture never imports an SDK or contacts a provider.
func cuaScriptImportSelection(t *testing.T, python, script string, payload bridgeRequest, env []string) []string {
	t.Helper()
	encodedScript, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := "import json\nns = {'__name__': 'contract'}\nexec(" + string(encodedScript) + ", ns)\ndef capture(preferred, fallback):\n    print(json.dumps([preferred, fallback]))\n    raise SystemExit(0)\nns['import_sdk'] = capture\nns['main']()\n"
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-I", "-c", bootstrap)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(encodedPayload)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("local Python selection: %v: %s", err, output)
	}
	var selected []string
	if err := json.Unmarshal(output, &selected); err != nil || len(selected) != 2 {
		t.Fatalf("selection=%s error=%v", output, err)
	}
	return selected
}

func TestBridgeTimeoutBoundsDoctorAndInventory(t *testing.T) {
	cfg := testConfig()
	cfg.Cua.ExecTimeoutSecs = 60
	if got := bridgeTimeout(cfg, bridgeRequest{Action: "doctor"}); got != 15*time.Second {
		t.Fatalf("doctor timeout=%s want 15s", got)
	}
	if got := bridgeTimeout(cfg, bridgeRequest{Action: "list"}); got != 60*time.Second {
		t.Fatalf("list timeout=%s want 60s", got)
	}
}

func TestBridgeRejectsMalformedJSON(t *testing.T) {
	runner := &recordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		_, _ = io.WriteString(req.Stdout, `not-json`)
		return core.LocalCommandResult{ExitCode: 0}, nil
	}}
	client := newBridgeClient(testConfig(), core.Runtime{Exec: runner})
	if _, err := client.RoundTrip(context.Background(), bridgeRequest{Action: "doctor"}); err == nil {
		t.Fatal("expected malformed JSON error")
	}
}

func TestBridgeScriptKeepsLifecycleActionsExplicitlyDispatched(t *testing.T) {
	for _, snippet := range []string{`action == "doctor"`, `action == "list"`, `action == "info"`, `action == "create"`, `action == "delete"`} {
		if !strings.Contains(bridgeScript, snippet) {
			t.Fatalf("bridge script missing %q", snippet)
		}
	}
	for _, forbidden := range []string{`action == "upload_bytes"`, `action == "exec"`, `def upload_bytes`, `def exec_command`, `cls.create(`, `def create(`, `cls.delete(`, `def delete(`} {
		if strings.Contains(bridgeScript, forbidden) {
			t.Fatalf("non-provisioning bridge retained unsupported path %q", forbidden)
		}
	}
	if !strings.Contains(bridgeScript, `auth=verified_read_only`) || !strings.Contains(bridgeScript, `inventory = list_sandboxes(mod)`) {
		t.Fatalf("doctor must verify auth with a read-only inventory probe")
	}
	if !strings.Contains(bridgeScript, `"code": "provisioning_disabled"`) || !strings.Contains(bridgeScript, cuaTrackingIssue) {
		t.Fatalf("bridge script must fail closed with the tracked upstream requirement")
	}
	if !strings.Contains(bridgeScript, `"code": "deletion_disabled"`) {
		t.Fatalf("bridge script must fail deletion closed")
	}
	if !strings.Contains(bridgeScript, `(3, 11) <= (major, minor) < (3, 14)`) || !strings.Contains(bridgeScript, `(3, 12) <= (major, minor) < (3, 14)`) {
		t.Fatalf("bridge script must enforce the supported Python range for each CUA SDK import")
	}
	if strings.Contains(bridgeScript, "CUA_API_BASE") {
		t.Fatalf("bridge script must use SDK CUA_BASE_URL, not CLI-only CUA_API_BASE")
	}
}

func TestEmbeddedBridgeRefusesAllMutation(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	moduleDir := t.TempDir()
	module := `
_exists = True

def configure(**kwargs):
    pass

class Info:
    name = "provider-assigned-123"
    status = "running"
    source = "cloud"
    os_type = "linux"
    created_at = "2026-07-03T12:00:00Z"

class HTTPStatusError(Exception):
    status_code = 404

class Sandbox:
    @classmethod
    async def create(cls, image, **kwargs):
        raise RuntimeError("BILLABLE CREATE ATTEMPTED")
    @classmethod
    async def get_info(cls, name):
        if name == "sandbox-404-timeout":
            raise RuntimeError("request to sandbox-404-timeout timed out")
        if name == "local-file-missing":
            raise FileNotFoundError("local SDK file missing")
        if name == "missing-sandbox":
            raise HTTPStatusError("missing")
        assert name == "provider-assigned-123"
        return Info()
    @classmethod
    async def delete(cls, name):
        global _exists
        assert name == "provider-assigned-123"
        _exists = False
    @classmethod
    async def list(cls):
        return [Info()] if _exists else []
`
	if err := os.WriteFile(filepath.Join(moduleDir, "cua.py"), []byte(module), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(payload string) bridgeResponse {
		t.Helper()
		cmd := exec.Command(python, "-c", bridgeScript)
		cmd.Env = []string{"PYTHONPATH=" + moduleDir, strings.Join([]string{"CUA_API_KEY", "placeholder"}, "=")}
		cmd.Stdin = strings.NewReader(payload)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bridge: %v\n%s", err, out)
		}
		var resp bridgeResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("decode bridge response: %v\n%s", err, out)
		}
		return resp
	}
	created := run(`{"version":1,"action":"create","config":{"sdkImport":"cua","image":"ubuntu:24.04","kind":"container"}}`)
	if created.OK || created.Error == nil || created.Error.Code != "provisioning_disabled" || strings.Contains(created.Error.Message, "BILLABLE CREATE ATTEMPTED") {
		t.Fatalf("create response=%#v", created)
	}
	doctor := run(`{"version":1,"action":"doctor","config":{"sdkImport":"cua"}}`)
	if doctor.Doctor.Auth != "env" || doctor.Doctor.Details["mutation"] != "false" || !doctorHasCheck(doctor, "auth", "ok") || !doctorHasCheck(doctor, "inventory", "ok") {
		t.Fatalf("doctor response=%#v", doctor)
	}
	listed := run(`{"version":1,"action":"list","config":{"sdkImport":"cua"}}`)
	if !listed.OK || len(listed.Sandboxes) != 1 || listed.Sandboxes[0].OSType != "linux" {
		t.Fatalf("list response=%#v", listed)
	}
	failedInfo := run(`{"version":1,"action":"info","sandboxId":"sandbox-404-timeout","config":{"sdkImport":"cua"}}`)
	if failedInfo.OK || failedInfo.Class == "not_found" || failedInfo.Error == nil || failedInfo.Error.Class == "not_found" {
		t.Fatalf("unstructured 404 text misclassified: %#v", failedInfo)
	}
	localFailure := run(`{"version":1,"action":"info","sandboxId":"local-file-missing","config":{"sdkImport":"cua"}}`)
	if localFailure.OK || localFailure.Class == "not_found" || localFailure.Error == nil || localFailure.Error.Class == "not_found" {
		t.Fatalf("local file error misclassified: %#v", localFailure)
	}
	missing := run(`{"version":1,"action":"info","sandboxId":"missing-sandbox","config":{"sdkImport":"cua"}}`)
	if missing.OK || missing.Class != "not_found" || missing.Error == nil || missing.Error.Class != "not_found" {
		t.Fatalf("structured HTTP 404 not classified: %#v", missing)
	}
	deleted := run(`{"version":1,"action":"delete","sandboxId":"provider-assigned-123","sandbox":{"metadata":{"createdAt":"2026-07-03T12:00:00Z"}},"config":{"sdkImport":"cua"}}`)
	if deleted.OK || deleted.Error == nil || deleted.Error.Code != "deletion_disabled" || strings.Contains(deleted.Error.Message, "UNSAFE DELETE ATTEMPTED") {
		t.Fatalf("delete response=%#v", deleted)
	}
}

func doctorHasCheck(resp bridgeResponse, name, status string) bool {
	for _, item := range resp.Doctor.Checks {
		if item.Check == name && item.Status == status {
			return true
		}
	}
	return false
}
