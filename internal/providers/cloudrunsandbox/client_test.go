package cloudrunsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestNewTransportRemoteAndDirect(t *testing.T) {
	// Remote requires secret env.
	t.Setenv("CLOUD_RUN_SANDBOX_SECRET", "")
	t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_SECRET", "")
	t.Setenv("CLOUD_RUN_AUTH_TOKEN", "")
	t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_AUTH_TOKEN", "")
	_, err := newTransport(Config{CloudRunSandbox: CloudRunSandboxConfig{
		GatewayURL: "https://gw.example.run.app",
		CLIPath:    defaultCLIPath,
	}}, Runtime{})
	if err == nil || !strings.Contains(err.Error(), "requires CLOUD_RUN_SANDBOX_SECRET") {
		t.Fatalf("expected secret requirement, got %v", err)
	}

	t.Setenv("CLOUD_RUN_SANDBOX_SECRET", "sec")
	t.Setenv("CRABBOX_CLOUD_RUN_SANDBOX_AUTH_TOKEN", "tok")
	transport, err := newTransport(Config{CloudRunSandbox: CloudRunSandboxConfig{
		GatewayURL: "https://gw.example.run.app/",
		CLIPath:    defaultCLIPath,
	}}, Runtime{})
	if err != nil {
		t.Fatalf("remote newTransport: %v", err)
	}
	remote, ok := transport.(*remoteTransport)
	if !ok || remote.Mode() != "remote" || remote.secret != "sec" || remote.authToken != "tok" {
		t.Fatalf("remote transport=%#v", transport)
	}
	if remote.baseURL != "https://gw.example.run.app" {
		t.Fatalf("baseURL=%q", remote.baseURL)
	}

	// Direct mode requires Runtime.Exec.
	_, err = newTransport(Config{CloudRunSandbox: CloudRunSandboxConfig{CLIPath: defaultCLIPath}}, Runtime{})
	if err == nil || !strings.Contains(err.Error(), "requires Runtime.Exec") {
		t.Fatalf("expected Exec requirement, got %v", err)
	}

	transport, err = newTransport(Config{CloudRunSandbox: CloudRunSandboxConfig{
		CLIPath: "/bin/sandbox",
		Mode:    "local",
	}}, Runtime{Exec: stubLocalExec{}})
	if err != nil {
		t.Fatalf("direct newTransport: %v", err)
	}
	if transport.Mode() != "direct" {
		t.Fatalf("mode=%s", transport.Mode())
	}
}

func TestRemoteTransportErrorPaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			http.Error(w, "nope", http.StatusInternalServerError)
		case "/v1/sandbox/create":
			http.Error(w, `{"error":"create failed"}`, http.StatusBadRequest)
		case "/v1/sandbox/exec":
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`not-json`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	transport := &remoteTransport{
		baseURL:   server.URL,
		secret:    "sec",
		authToken: "tok",
		http:      server.Client(),
		cfg:       Config{CloudRunSandbox: CloudRunSandboxConfig{Write: true}},
	}
	ctx := context.Background()
	if err := transport.Health(ctx); err == nil {
		t.Fatal("expected health error")
	}
	if err := transport.Create(ctx, "bad id!", runOptions{}); err == nil {
		t.Fatal("expected invalid id")
	}
	if err := transport.Create(ctx, "ok-id", runOptions{Env: map[string]string{"BAD-KEY": "x"}}); err == nil {
		t.Fatal("expected invalid env")
	}
	if err := transport.Create(ctx, "ok-id", runOptions{}); err == nil || !strings.Contains(err.Error(), "create failed") {
		t.Fatalf("expected create failure, got %v", err)
	}
	code, err := transport.Exec(ctx, "ok-id", "echo", execOptions{}, nil, nil)
	if err == nil || code != 1 {
		t.Fatalf("expected decode error code=%d err=%v", code, err)
	}

	// Unauthorized path.
	transport.authToken = "wrong"
	_, err = transport.Exec(ctx, "ok-id", "echo", execOptions{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("expected unauthorized, got %v", err)
	}

	// Health soft-success for 404.
	server404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(server404.Close)
	transport404 := &remoteTransport{baseURL: server404.URL, secret: "sec", http: server404.Client()}
	if err := transport404.Health(context.Background()); err != nil {
		t.Fatalf("404 health should be ok: %v", err)
	}
}

func TestDirectTransportLifecycle(t *testing.T) {
	var calls []string
	exec := recordingLocalExec{handler: func(req LocalCommandRequest) (LocalCommandResult, error) {
		joined := strings.Join(append([]string{req.Name}, req.Args...), " ")
		calls = append(calls, joined)
		if strings.Contains(joined, "--help") {
			return LocalCommandResult{ExitCode: 0}, nil
		}
		if strings.Contains(joined, " delete ") {
			if strings.Contains(joined, "missing-box") {
				return LocalCommandResult{ExitCode: 1}, errors.New("sandbox not found")
			}
			return LocalCommandResult{ExitCode: 0}, nil
		}
		if strings.Contains(joined, " run ") || strings.Contains(joined, " exec ") {
			if req.Stdout != nil {
				_, _ = io.WriteString(req.Stdout, "ok\n")
			}
			return LocalCommandResult{ExitCode: 0}, nil
		}
		return LocalCommandResult{ExitCode: 0}, nil
	}}
	transport := &directTransport{
		cfg: Config{CloudRunSandbox: CloudRunSandboxConfig{
			CLIPath:     "/bin/sandbox",
			Mode:        "container",
			AllowEgress: true,
			Write:       true,
			Rootfs:      "/",
			Workdir:     "/tmp/crabbox",
		}},
		rt: Runtime{Exec: exec},
	}
	ctx := context.Background()
	if err := transport.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if err := transport.Create(ctx, "box1", runOptions{Env: map[string]string{"A": "1"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	code, err := transport.Exec(ctx, "box1", "echo hi", execOptions{Workdir: "/tmp/crabbox", Env: map[string]string{"B": "2"}}, io.Discard, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("Exec: code=%d err=%v", code, err)
	}
	if err := transport.WriteFile(ctx, "box1", "/tmp/x", "data"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := transport.Destroy(ctx, "box1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := transport.Destroy(ctx, "missing-box"); err != nil {
		t.Fatalf("Destroy missing should ignore not-found: %v", err)
	}
	if len(calls) < 5 {
		t.Fatalf("calls=%v", calls)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "--mode container") || !strings.Contains(joined, "--allow-egress") || !strings.Contains(joined, "--write") {
		t.Fatalf("missing run flags in calls:\n%s", joined)
	}
}

func TestSecureHTTPClientSameOrigin(t *testing.T) {
	t.Parallel()
	trusted, _ := url.Parse("https://gw.example.run.app")
	other, _ := url.Parse("https://evil.example")
	if !sameOrigin(trusted, trusted) || sameOrigin(trusted, other) {
		t.Fatal("sameOrigin mismatch")
	}
	if effectivePort(trusted) != "443" {
		t.Fatalf("https port=%s", effectivePort(trusted))
	}
	httpURL, _ := url.Parse("http://127.0.0.1")
	if effectivePort(httpURL) != "80" {
		t.Fatalf("http port=%s", effectivePort(httpURL))
	}
	client := secureHTTPClient(http.DefaultClient, "https://gw.example.run.app")
	if client.CheckRedirect == nil {
		t.Fatal("expected CheckRedirect wrapper")
	}
}

type stubLocalExec struct{}

func (stubLocalExec) Run(context.Context, LocalCommandRequest) (LocalCommandResult, error) {
	return LocalCommandResult{}, nil
}

type recordingLocalExec struct {
	handler func(LocalCommandRequest) (LocalCommandResult, error)
}

func (r recordingLocalExec) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	if r.handler != nil {
		return r.handler(req)
	}
	return LocalCommandResult{}, nil
}
