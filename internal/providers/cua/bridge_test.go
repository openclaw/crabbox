package cua

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	calls []LocalCommandRequest
	fn    func(LocalCommandRequest) (LocalCommandResult, error)
}

func (r *recordingRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.fn != nil {
		return r.fn(req)
	}
	return LocalCommandResult{ExitCode: 0}, nil
}

func (r *recordingRunner) onlyCall(t *testing.T) LocalCommandRequest {
	t.Helper()
	if len(r.calls) != 1 {
		t.Fatalf("calls=%d want 1", len(r.calls))
	}
	return r.calls[0]
}

func testConfig() Config {
	return Config{Provider: providerName, Cua: core.CuaConfig{
		APIURL:            "https://api.cua.example/v1/",
		Image:             defaultImage,
		Kind:              defaultKind,
		Workdir:           defaultWorkdir,
		ExecTimeoutSecs:   60,
		BridgeCommand:     defaultBridgeCommand,
		SDKPackage:        defaultSDKPackage,
		SDKImport:         defaultSDKImport,
		SDKFallbackImport: defaultSDKFallbackImport,
	}}
}

func requestBody(t *testing.T, req LocalCommandRequest) string {
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
	secret := "cua-secret-value"
	t.Setenv("CRABBOX_CUA_API_KEY", secret)
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret")
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		for _, arg := range req.Args {
			if strings.Contains(arg, secret) {
				t.Fatalf("secret leaked into argv: %#v", req.Args)
			}
		}
		if !envContains(req.Env, "CUA_API_KEY="+secret) {
			t.Fatalf("bridge env missing SDK API key: %#v", req.Env)
		}
		if envContains(req.Env, "AWS_SECRET_ACCESS_KEY=ambient-secret") {
			t.Fatalf("bridge env inherited unrelated ambient secret: %#v", req.Env)
		}
		if strings.TrimSpace(req.Dir) == "" || !strings.Contains(req.Dir, "cua-bridge") {
			t.Fatalf("bridge must run from a trusted cache directory, got %q", req.Dir)
		}
		var payload bridgeRequest
		if err := json.Unmarshal([]byte(requestBody(t, req)), &payload); err != nil {
			t.Fatalf("stdin payload: %v", err)
		}
		if payload.Action != "doctor" || payload.Version != bridgeVersion || payload.Config.APIURL != "https://api.cua.example/v1" || payload.Config.ExecTimeout != 60 {
			t.Fatalf("payload=%#v", payload)
		}
		_, _ = io.WriteString(req.Stdout, `{"ok":true,"doctor":{"importPath":"cua","auth":"env","checks":[{"status":"ok","check":"sdk"}]}}`)
		return LocalCommandResult{ExitCode: 0}, nil
	}}
	client := newBridgeClient(testConfig(), Runtime{Exec: runner})
	resp, err := client.RoundTrip(context.Background(), bridgeRequest{Action: "doctor"})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if !resp.OK || resp.Doctor.ImportPath != "cua" {
		t.Fatalf("resp=%#v", resp)
	}
	call := runner.onlyCall(t)
	if call.Name != defaultBridgeCommand {
		t.Fatalf("command=%q", call.Name)
	}
	if !reflect.DeepEqual(call.Args[:1], []string{"-c"}) {
		t.Fatalf("args=%#v", call.Args)
	}
	if !strings.Contains(call.Args[1], "def doctor") {
		t.Fatalf("embedded script missing doctor implementation")
	}
}

func TestBridgeRedactsSecretFromCommandFailure(t *testing.T) {
	secret := "cua-secret-value"
	t.Setenv("CRABBOX_CUA_API_KEY", secret)
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		_, _ = io.WriteString(req.Stderr, "denied "+secret)
		return LocalCommandResult{ExitCode: 1}, errors.New("exit status 1")
	}}
	client := newBridgeClient(testConfig(), Runtime{Exec: runner})
	_, err := client.RoundTrip(context.Background(), bridgeRequest{Action: "doctor"})
	if err == nil {
		t.Fatal("expected bridge failure")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error was not redacted: %v", err)
	}
}

func TestBridgeExecUsesRunOutputCaptureLimit(t *testing.T) {
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if req.MaxCapturedOutputBytes != bridgeExecOutputLimit {
			t.Fatalf("exec output limit=%d want %d", req.MaxCapturedOutputBytes, bridgeExecOutputLimit)
		}
		_, _ = io.WriteString(req.Stdout, `{"ok":true,"exitCode":0}`)
		return LocalCommandResult{ExitCode: 0}, nil
	}}
	client := newBridgeClient(testConfig(), Runtime{Exec: runner})
	if _, err := client.RoundTrip(context.Background(), bridgeRequest{Action: "exec", SandboxID: "crabbox-cua-test", Command: []string{"true"}}); err != nil {
		t.Fatalf("RoundTrip exec: %v", err)
	}
}

func TestBridgeTimeoutUsesStartupTimeoutForCreate(t *testing.T) {
	cfg := testConfig()
	cfg.Cua.ExecTimeoutSecs = 60
	cfg.Cua.StartupTimeoutSecs = 300
	if got := bridgeTimeout(cfg, bridgeRequest{Action: "create"}); got != 300*time.Second {
		t.Fatalf("create timeout=%s want 300s", got)
	}
	if got := bridgeTimeout(cfg, bridgeRequest{Action: "exec"}); got != 60*time.Second {
		t.Fatalf("exec timeout=%s want 60s", got)
	}
	if got := bridgeTimeout(cfg, bridgeRequest{Action: "create", Timeout: 45}); got != 45*time.Second {
		t.Fatalf("explicit timeout=%s want 45s", got)
	}
}

func TestBridgeRejectsMalformedJSON(t *testing.T) {
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		_, _ = io.WriteString(req.Stdout, `not-json`)
		return LocalCommandResult{ExitCode: 0}, nil
	}}
	client := newBridgeClient(testConfig(), Runtime{Exec: runner})
	if _, err := client.RoundTrip(context.Background(), bridgeRequest{Action: "doctor"}); err == nil {
		t.Fatal("expected malformed JSON error")
	}
}

func TestBridgeScriptKeepsLifecycleActionsExplicitlyDispatched(t *testing.T) {
	for _, snippet := range []string{`action == "doctor"`, `action == "list"`, `action == "info"`, `action == "create"`, `action == "delete"`, `action == "upload_bytes"`, `action == "exec"`} {
		if !strings.Contains(bridgeScript, snippet) {
			t.Fatalf("bridge script missing %q", snippet)
		}
	}
	if !strings.Contains(bridgeScript, `env_file_content(env)`) || !strings.Contains(bridgeScript, `sb.files.write_bytes(env_file_path`) || !strings.Contains(bridgeScript, `. \"$env_file\"`) {
		t.Fatalf("bridge script must materialize forwarded env outside the command text")
	}
	if !strings.Contains(bridgeScript, `cleanup_env_file(sb, env_file_path)`) || !strings.Contains(bridgeScript, `await remove(path)`) {
		t.Fatalf("bridge script must remove forwarded env files outside the user command trap")
	}
	if strings.Contains(bridgeScript, `env_prefix`) || strings.Contains(bridgeScript, `f"{k}={shlex.quote(v)}"`) {
		t.Fatalf("bridge script must not embed forwarded env values in the command text")
	}
	if strings.Contains(bridgeScript, `if len(values) == 1`) {
		t.Fatalf("bridge script must not execute single-argument argv as raw shell text")
	}
	if !strings.Contains(bridgeScript, `invalid_env`) || !strings.Contains(bridgeScript, `valid_env_name`) {
		t.Fatalf("bridge script must validate forwarded env names before shell construction")
	}
	if !strings.Contains(bridgeScript, `inspect.signature(create_func).parameters`) || !strings.Contains(bridgeScript, `"time_to_start" in parameters`) {
		t.Fatalf("bridge script must guard SDK-specific startup timeout kwargs")
	}
	if strings.Contains(bridgeScript, "CUA_API_BASE") {
		t.Fatalf("bridge script must use SDK CUA_BASE_URL, not CLI-only CUA_API_BASE")
	}
}
