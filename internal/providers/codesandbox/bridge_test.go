package codesandbox

import (
	"context"
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
