package cloudrunsandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateGatewayURL(t *testing.T) {
	t.Parallel()
	got, err := validateGatewayURL("https://example.run.app/")
	if err != nil {
		t.Fatalf("validateGatewayURL: %v", err)
	}
	if got != "https://example.run.app" {
		t.Fatalf("got %q", got)
	}
	if _, err := validateGatewayURL("http://evil.example"); err == nil {
		t.Fatal("expected non-loopback http rejection")
	}
	if _, err := validateGatewayURL("https://user:pass@example.run.app"); err == nil {
		t.Fatal("expected userinfo rejection")
	}
	if _, err := validateGatewayURL("http://127.0.0.1:8080"); err != nil {
		t.Fatalf("loopback http should be allowed: %v", err)
	}
}

func TestClientHelpers(t *testing.T) {
	t.Parallel()
	if got := firstNonEmpty("", "  ", " value ", "later"); got != "value" {
		t.Fatalf("firstNonEmpty=%q", got)
	}
	if !isLoopbackHost("localhost") || !isLoopbackHost("::1") || isLoopbackHost("example.com") {
		t.Fatal("unexpected loopback classification")
	}
	if err := validateEnv(map[string]string{"VALID_1": "x"}); err != nil {
		t.Fatalf("valid env rejected: %v", err)
	}
	if err := validateEnv(map[string]string{"INVALID-NAME": "x"}); err == nil {
		t.Fatal("expected invalid env rejection")
	}
	if !isNotFoundDetail("resource does not exist") || isNotFoundDetail("permission denied") {
		t.Fatal("unexpected not-found classification")
	}
}

func TestRemoteRequestBody(t *testing.T) {
	t.Parallel()
	transport := &remoteTransport{cfg: Config{CloudRunSandbox: CloudRunSandboxConfig{
		AllowEgress: true,
		Write:       true,
		Rootfs:      "base",
		Workdir:     "/default",
	}}}
	body := transport.requestBody("box", runOptions{
		Rootfs:  "override",
		Workdir: "/work",
		Env:     map[string]string{"A": "B"},
	}, map[string]any{"command": "echo ok"})
	if body["sandboxId"] != "box" || body["executionMode"] != "stateful" || body["allowEgress"] != true || body["write"] != true {
		t.Fatalf("unexpected base body: %#v", body)
	}
	if body["rootfs"] != "override" || body["workdir"] != "/work" || body["cwd"] != "/work" || body["command"] != "echo ok" {
		t.Fatalf("unexpected optional body: %#v", body)
	}
}

func TestRemoteTransportLifecycle(t *testing.T) {
	t.Parallel()
	var creates, execs, destroys, writes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" && r.Header.Get("X-ComputeSDK-Cloud-Run-Secret") != "test-secret" {
			http.Error(w, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/v1/sandbox/create":
			creates++
			if payload["sandboxId"] != "crabbox-demo-abc123" {
				t.Errorf("create sandboxId=%v", payload["sandboxId"])
			}
			if payload["executionMode"] != "stateful" {
				t.Errorf("create executionMode=%v", payload["executionMode"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxId": payload["sandboxId"], "status": "running"})
		case "/v1/sandbox/exec":
			execs++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"exitCode": 0,
				"stdout":   "ok\n",
				"stderr":   "",
			})
		case "/v1/sandbox/writeFile":
			writes++
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		case "/v1/sandbox/destroy":
			destroys++
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	transport := &remoteTransport{
		baseURL: server.URL,
		secret:  "test-secret",
		cfg: Config{
			CloudRunSandbox: CloudRunSandboxConfig{
				GatewayURL: server.URL,
				CLIPath:    defaultCLIPath,
				Workdir:    defaultWorkdir,
				Write:      true,
			},
		},
		http: server.Client(),
	}
	ctx := context.Background()
	if err := transport.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if err := transport.Create(ctx, "crabbox-demo-abc123", runOptions{Write: true, Workdir: "/tmp/crabbox"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var stdout strings.Builder
	code, err := transport.Exec(ctx, "crabbox-demo-abc123", "echo ok", execOptions{Workdir: "/tmp/crabbox"}, &stdout, nil)
	if err != nil || code != 0 {
		t.Fatalf("Exec: code=%d err=%v", code, err)
	}
	if stdout.String() != "ok\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if err := transport.WriteFile(ctx, "crabbox-demo-abc123", "/tmp/x", "hello"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := transport.Destroy(ctx, "crabbox-demo-abc123"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if creates != 1 || execs != 1 || writes != 1 || destroys != 1 {
		t.Fatalf("counts create=%d exec=%d write=%d destroy=%d", creates, execs, writes, destroys)
	}
}

func TestValidateSandboxID(t *testing.T) {
	t.Parallel()
	if err := validateSandboxID("crabbox-demo-abc123"); err != nil {
		t.Fatalf("valid id rejected: %v", err)
	}
	if err := validateSandboxID("../etc"); err == nil {
		t.Fatal("expected path-like id rejection")
	}
}
