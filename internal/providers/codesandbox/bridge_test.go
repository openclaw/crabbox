package codesandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestSDKBridgeSendsJSONOnStdinAndTokenOnlyInEnv(t *testing.T) {
	secret := "csb-secret-value"
	runner := &recordingBridgeRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		for _, arg := range req.Args {
			if strings.Contains(arg, secret) {
				t.Fatalf("secret leaked into argv: %#v", req.Args)
			}
		}
		if !envContains(req.Env, codesandboxFallbackAPIKeyEnv+"="+secret) {
			t.Fatalf("bridge env missing SDK auth token")
		}
		if !envContains(req.Env, "CRABBOX_CODESANDBOX_SDK_PACKAGE=@codesandbox/sdk") {
			t.Fatalf("bridge env missing SDK package")
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
	call := runner.onlyCall(t)
	if call.Name != "node" {
		t.Fatalf("command=%q", call.Name)
	}
	if !reflect.DeepEqual(call.Args[:2], []string{"--input-type=module", "-e"}) {
		t.Fatalf("args=%#v", call.Args)
	}
}

func TestSDKBridgeRedactsTokenFromCommandFailures(t *testing.T) {
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
}

func TestSDKBridgeClassifiesMalformedJSON(t *testing.T) {
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
	calls []LocalCommandRequest
	fn    func(LocalCommandRequest) (LocalCommandResult, error)
}

func (r *recordingBridgeRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.fn != nil {
		return r.fn(req)
	}
	return LocalCommandResult{ExitCode: 0}, nil
}

func (r *recordingBridgeRunner) onlyCall(t *testing.T) LocalCommandRequest {
	t.Helper()
	if len(r.calls) != 1 {
		t.Fatalf("calls=%d want 1", len(r.calls))
	}
	return r.calls[0]
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

var _ core.CommandRunner = (*recordingBridgeRunner)(nil)
