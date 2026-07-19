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
	"time"
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
	if !isDirectSandboxNotFound("box-1", "sandbox box-1 does not exist") || isDirectSandboxNotFound("box-1", "fork/exec: no such file or directory") {
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
		Rootfs:         "override",
		Workdir:        "/work",
		OwnershipToken: "box",
		Env:            map[string]string{"A": "B"},
	}, map[string]any{"command": "echo ok"})
	if body["sandboxId"] != "box" || body["ownershipToken"] != "box" || body["executionMode"] != "stateful" || body["allowEgress"] != true || body["write"] != true {
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
		if r.Header.Get("X-ComputeSDK-Cloud-Run-Secret") != "test-secret" {
			http.Error(w, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":    "ok",
				"lifecycle": map[string]string{"routing": "durable", "destroy": "synchronous", "exec": "ndjson-stream"},
			})
		case "/v1/sandbox/create":
			creates++
			if payload["sandboxId"] != "crabbox-demo-abc123" {
				t.Errorf("create sandboxId=%v", payload["sandboxId"])
			}
			if payload["executionMode"] != "stateful" {
				t.Errorf("create executionMode=%v", payload["executionMode"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sandboxId":      payload["sandboxId"],
				"ownershipToken": payload["ownershipToken"],
				"status":         "running",
				"lifecycle":      map[string]string{"routing": "durable", "destroy": "synchronous", "exec": "ndjson-stream"},
			})
		case "/v1/sandbox/exec":
			execs++
			w.Header().Set("Content-Type", "application/x-ndjson")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sandboxId": payload["sandboxId"],
				"stream":    "stdout",
				"data":      "ok\n",
			})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sandboxId": payload["sandboxId"],
				"status":    "completed",
				"success":   true,
				"exitCode":  0,
			})
		case "/v1/sandbox/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "sandboxId": payload["sandboxId"], "ownershipToken": payload["ownershipToken"], "status": "running"})
		case "/v1/sandbox/writeFile":
			writes++
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "sandboxId": payload["sandboxId"], "status": "written"})
		case "/v1/sandbox/destroy":
			destroys++
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "sandboxId": payload["sandboxId"], "ownershipToken": payload["ownershipToken"], "status": "destroyed"})
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
	if err := transport.Create(ctx, "crabbox-demo-abc123", runOptions{Write: true, Workdir: "/tmp/crabbox", OwnershipToken: "crabbox-demo-abc123"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := transport.Probe(ctx, "crabbox-demo-abc123", "crabbox-demo-abc123"); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	var stdout strings.Builder
	code, err := transport.Exec(ctx, "crabbox-demo-abc123", "echo ok", execOptions{Workdir: "/tmp/crabbox"}, &stdout, nil)
	if err != nil || code != 0 {
		t.Fatalf("Exec: code=%d err=%v", code, err)
	}
	if stdout.String() != "ok\n" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if err := transport.WriteFile(ctx, "crabbox-demo-abc123", "/tmp/x", "hello", false); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := transport.Destroy(ctx, "crabbox-demo-abc123", "crabbox-demo-abc123"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if creates != 1 || execs != 1 || writes != 1 || destroys != 1 {
		t.Fatalf("counts create=%d exec=%d write=%d destroy=%d", creates, execs, writes, destroys)
	}
}

func TestRemoteTransportRejectsTwoHundredExecFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sandboxId": "box",
			"status":    "failed",
			"success":   false,
			"error":     "execution unavailable",
		})
	}))
	t.Cleanup(server.Close)
	transport := &remoteTransport{baseURL: server.URL, secret: "sec", http: server.Client()}
	code, err := transport.Exec(context.Background(), "box", "true", execOptions{}, nil, nil)
	if err == nil || code != 1 || !strings.Contains(err.Error(), "execution unavailable") {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestRemoteTransportPropagatesOutputWriteFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxId": "box", "stream": "stdout", "data": "output"})
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxId": "box", "status": "completed", "success": true, "exitCode": 0})
	}))
	t.Cleanup(server.Close)
	wantErr := errors.New("output unavailable")
	transport := &remoteTransport{baseURL: server.URL, secret: "sec", http: server.Client()}
	code, err := transport.Exec(context.Background(), "box", "true", execOptions{}, errorWriter{err: wantErr}, nil)
	if code != 1 || !errors.Is(err, wantErr) {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

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
			w.Header().Set("Content-Type", "application/x-ndjson")
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
	if err := transport.Create(ctx, "ok-id", runOptions{OwnershipToken: "ok-id"}); err == nil || !strings.Contains(err.Error(), "create failed") {
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

	// A missing health route cannot prove the required lifecycle contract.
	server404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(server404.Close)
	transport404 := &remoteTransport{baseURL: server404.URL, secret: "sec", http: server404.Client()}
	if err := transport404.Health(context.Background()); err == nil {
		t.Fatal("404 health should fail closed")
	}
}

func TestRemoteTransportRequiresDurableLifecycleConfirmations(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/v1/sandbox/create":
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxId": "box", "ownershipToken": "box", "status": "running"})
		case "/v1/sandbox/destroy":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	transport := &remoteTransport{baseURL: server.URL, secret: "sec", http: server.Client()}
	if err := transport.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "routing=durable") {
		t.Fatalf("health error=%v", err)
	}
	if err := transport.Create(context.Background(), "box", runOptions{OwnershipToken: "box"}); err == nil || !strings.Contains(err.Error(), "routing=durable") {
		t.Fatalf("create error=%v", err)
	}
	if err := transport.Destroy(context.Background(), "box", "box"); err == nil || !strings.Contains(err.Error(), "did not synchronously confirm") {
		t.Fatalf("destroy error=%v", err)
	}
}

func TestRemoteTransportOnlyAcceptsStructuredSandboxNotFound(t *testing.T) {
	t.Parallel()
	structured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":          "sandbox box missing",
			"code":           "sandbox_not_found",
			"sandboxId":      "box",
			"ownershipToken": "box",
		})
	}))
	t.Cleanup(structured.Close)
	transport := &remoteTransport{baseURL: structured.URL, secret: "sec", http: structured.Client()}
	if err := transport.Destroy(context.Background(), "box", "box"); !errors.Is(err, errSandboxNotFound) {
		t.Fatalf("structured not found=%v", err)
	}
	if err := transport.Destroy(context.Background(), "box", "other-token"); err == nil || errors.Is(err, errSandboxNotFound) {
		t.Fatalf("mismatched ownership token must not prove absence: %v", err)
	}

	generic := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(generic.Close)
	transport = &remoteTransport{baseURL: generic.URL, secret: "sec", http: generic.Client()}
	if err := transport.Destroy(context.Background(), "box", "box"); err == nil || errors.Is(err, errSandboxNotFound) {
		t.Fatalf("generic route 404=%v", err)
	}
}

func TestRemoteTransportOnlyAcceptsStructuredCreateConflict(t *testing.T) {
	t.Parallel()
	structured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":          "sandbox box already exists",
			"code":           "sandbox_already_exists",
			"sandboxId":      "box",
			"ownershipToken": "box",
		})
	}))
	t.Cleanup(structured.Close)
	transport := &remoteTransport{baseURL: structured.URL, secret: "sec", http: structured.Client()}
	if err := transport.Create(context.Background(), "box", runOptions{OwnershipToken: "box"}); !errors.Is(err, errSandboxAlreadyExists) {
		t.Fatalf("structured conflict=%v", err)
	}
	if err := transport.Create(context.Background(), "box", runOptions{OwnershipToken: "other-token"}); err == nil || errors.Is(err, errSandboxAlreadyExists) {
		t.Fatalf("mismatched ownership token must remain indeterminate: %v", err)
	}

	generic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "conflict", http.StatusConflict)
	}))
	t.Cleanup(generic.Close)
	transport = &remoteTransport{baseURL: generic.URL, secret: "sec", http: generic.Client()}
	if err := transport.Create(context.Background(), "box", runOptions{OwnershipToken: "box"}); err == nil || errors.Is(err, errSandboxAlreadyExists) {
		t.Fatalf("generic conflict must remain indeterminate: %v", err)
	}
}

func TestRemoteTransportBoundsControlRequests(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if _, ok := req.Context().Deadline(); !ok {
			t.Fatal("remote control request has no deadline")
		}
		body := `{"status":"ok","lifecycle":{"routing":"durable","destroy":"synchronous","exec":"ndjson-stream"}}`
		if req.URL.Path == "/v1/sandbox/destroy" {
			body = `{"success":true,"sandboxId":"box","ownershipToken":"box","status":"destroyed"}`
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	transport := &remoteTransport{baseURL: "https://gateway.example", secret: "sec", http: client}
	if err := transport.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := transport.Destroy(context.Background(), "box", "box"); err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

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
				_, _ = io.WriteString(req.Stderr, "sandbox missing-box not found")
				return LocalCommandResult{ExitCode: 1}, nil
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
	if err := transport.Create(ctx, "box1", runOptions{OwnershipToken: "box1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := transport.Probe(ctx, "box1", "box1"); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if err := transport.Create(ctx, "box2", runOptions{OwnershipToken: "box2", Env: map[string]string{"A": "1"}}); err == nil {
		t.Fatal("expected direct create environment rejection")
	}
	code, err := transport.Exec(ctx, "box1", "echo hi", execOptions{Workdir: "/tmp/crabbox", Env: map[string]string{"B": "2"}}, io.Discard, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("Exec: code=%d err=%v", code, err)
	}
	if err := transport.WriteFile(ctx, "box1", "/tmp/x", "data", false); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := transport.Destroy(ctx, "box1", "box1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if err := transport.Destroy(ctx, "missing-box", "missing-box"); !errors.Is(err, errSandboxNotFound) {
		t.Fatalf("Destroy missing error=%v", err)
	}
	if len(calls) < 5 {
		t.Fatalf("calls=%v", calls)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "--allow-egress") || !strings.Contains(joined, "--write") {
		t.Fatalf("missing run flags in calls:\n%s", joined)
	}
	if !strings.Contains(joined, "while :; do /bin/sleep 3600; done") {
		t.Fatalf("direct create did not launch an idle keeper:\n%s", joined)
	}
	for _, call := range calls {
		if strings.Contains(call, " run ") && strings.Contains(call, "--workdir") {
			t.Fatalf("direct create used workdir before it exists: %s", call)
		}
	}
}

func TestDirectTransportExecHonorsTimeout(t *testing.T) {
	t.Parallel()
	transport := &directTransport{
		cfg: Config{CloudRunSandbox: CloudRunSandboxConfig{CLIPath: "/bin/sandbox"}},
		rt: Runtime{Exec: contextLocalExec{run: func(ctx context.Context, _ LocalCommandRequest) (LocalCommandResult, error) {
			<-ctx.Done()
			return LocalCommandResult{ExitCode: 124}, ctx.Err()
		}}},
	}
	started := time.Now()
	code, err := transport.Exec(context.Background(), "box", "sleep 10", execOptions{Timeout: 20 * time.Millisecond}, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || code != 124 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestDirectTransportBoundsControlCommands(t *testing.T) {
	t.Parallel()
	calls := 0
	transport := &directTransport{
		cfg: Config{CloudRunSandbox: CloudRunSandboxConfig{CLIPath: "/bin/sandbox"}},
		rt: Runtime{Exec: contextLocalExec{run: func(ctx context.Context, _ LocalCommandRequest) (LocalCommandResult, error) {
			calls++
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("direct CLI call has no deadline")
			}
			return LocalCommandResult{}, nil
		}}},
	}
	ctx := context.Background()
	if err := transport.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if err := transport.Create(ctx, "box", runOptions{OwnershipToken: "box"}); err != nil {
		t.Fatal(err)
	}
	if err := transport.Destroy(ctx, "box", "box"); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestDirectTransportWriteFileStreamsLargePayloadOnStdin(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("sensitive-workspace-data", 8<<10)
	var request LocalCommandRequest
	transport := &directTransport{
		cfg: Config{CloudRunSandbox: CloudRunSandboxConfig{CLIPath: "/bin/sandbox"}},
		rt: Runtime{Exec: recordingLocalExec{handler: func(req LocalCommandRequest) (LocalCommandResult, error) {
			request = req
			return LocalCommandResult{}, nil
		}}},
	}
	if err := transport.WriteFile(context.Background(), "box", "/tmp/archive.b64", payload, false); err != nil {
		t.Fatal(err)
	}
	if request.Stdin == nil {
		t.Fatal("write payload was not provided on stdin")
	}
	if !request.DisableOutputCapture {
		t.Fatal("writeFile must not duplicate streamed output in command capture")
	}
	got, err := io.ReadAll(request.Stdin)
	if err != nil || string(got) != payload {
		t.Fatalf("stdin payload mismatch: bytes=%d err=%v", len(got), err)
	}
	if strings.Contains(strings.Join(request.Args, " "), "sensitive-workspace-data") {
		t.Fatalf("workspace content leaked into argv: %v", request.Args)
	}
}

func TestDirectTransportExecStreamsEnvironmentOnStdin(t *testing.T) {
	t.Parallel()
	var request LocalCommandRequest
	transport := &directTransport{
		cfg: Config{CloudRunSandbox: CloudRunSandboxConfig{CLIPath: "/bin/sandbox"}},
		rt: Runtime{Exec: recordingLocalExec{handler: func(req LocalCommandRequest) (LocalCommandResult, error) {
			request = req
			return LocalCommandResult{}, nil
		}}},
	}
	const value = "sensitive-env-value"
	if _, err := transport.Exec(context.Background(), "box", "true", execOptions{Env: map[string]string{"TOKEN": value}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(request.Args, " "), value) {
		t.Fatalf("environment value leaked into argv: %v", request.Args)
	}
	if !request.DisableOutputCapture {
		t.Fatal("exec must not duplicate streamed output in command capture")
	}
	script, err := io.ReadAll(request.Stdin)
	if err != nil || !strings.Contains(string(script), value) {
		t.Fatalf("stdin script=%q err=%v", script, err)
	}
	if !strings.Contains(string(script), "export PATH="+shellQuote(defaultSandboxPath)) {
		t.Fatalf("stdin script has no baseline PATH: %q", script)
	}
	if _, err := transport.Exec(context.Background(), "box", "true", execOptions{Env: map[string]string{"PATH": "/custom/bin"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	overrideScript, err := io.ReadAll(request.Stdin)
	if err != nil || !strings.Contains(string(overrideScript), "export PATH='/custom/bin'") || strings.Contains(string(overrideScript), defaultSandboxPath) {
		t.Fatalf("override script=%q err=%v", overrideScript, err)
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

type contextLocalExec struct {
	run func(context.Context, LocalCommandRequest) (LocalCommandResult, error)
}

func (r contextLocalExec) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	return r.run(ctx, req)
}

func (r recordingLocalExec) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	if r.handler != nil {
		return r.handler(req)
	}
	return LocalCommandResult{}, nil
}
