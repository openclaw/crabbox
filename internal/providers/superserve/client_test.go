package superserve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSuperserveClientCreateListActivateAndDelete(t *testing.T) {
	var requests []struct {
		method string
		path   string
		query  string
		body   string
		key    string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, struct {
			method string
			path   string
			query  string
			body   string
			key    string
		}{r.Method, r.URL.Path, r.URL.RawQuery, string(body), r.Header.Get("X-API-Key")})
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected Authorization header: %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			writeTestJSON(w, map[string]any{"id": "sb_123", "status": "running"})
		case r.Method == http.MethodGet && r.URL.Path == "/sandboxes":
			if r.URL.Query().Get("metadata."+metadataProviderKey) != providerName || r.URL.Query().Get("metadata."+metadataEndpointKey) == "" {
				t.Fatalf("metadata filters missing: %s", r.URL.RawQuery)
			}
			writeTestJSON(w, []map[string]any{{"id": "sb_123", "status": "running", "metadata": map[string]string{metadataProviderKey: providerName}}})
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/sb_123/activate":
			writeTestJSON(w, map[string]any{"access_token": "ss_test_token", "sandbox": map[string]any{"id": "sb_123", "status": "running"}})
		case r.Method == http.MethodPatch && r.URL.Path == "/sandboxes/sb_123":
			writeTestJSON(w, map[string]any{"id": "sb_123", "metadata": map[string]string{metadataProviderKey: providerName}})
		case r.Method == http.MethodDelete && r.URL.Path == "/sandboxes/sb_123":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateSandbox(context.Background(), createSandboxRequest{
		Name:           "crabbox-my-app",
		FromTemplate:   "superserve/base",
		FromSnapshot:   "snap_123",
		TimeoutSeconds: 300,
		Metadata:       map[string]string{metadataProviderKey: providerName},
		Network:        &createSandboxNetworkCfg{AllowOut: []string{"api.example.test"}, DenyOut: []string{"metadata.example.test"}},
	}); err != nil {
		t.Fatalf("CreateSandbox err=%v", err)
	}
	var createBody map[string]any
	if err := json.Unmarshal([]byte(requests[0].body), &createBody); err != nil {
		t.Fatalf("create body json=%q err=%v", requests[0].body, err)
	}
	if createBody["name"] != "crabbox-my-app" || createBody["from_template"] != "superserve/base" || createBody["from_snapshot"] != "snap_123" || createBody["timeout_seconds"].(float64) != 300 {
		t.Fatalf("create body used wrong API fields: %#v", createBody)
	}
	network, ok := createBody["network"].(map[string]any)
	if !ok || len(network["allow_out"].([]any)) != 1 || len(network["deny_out"].([]any)) != 1 {
		t.Fatalf("create network body=%#v", createBody["network"])
	}
	if _, ok := createBody["template"]; ok {
		t.Fatalf("create body sent legacy template field: %#v", createBody)
	}
	if _, err := client.ListSandboxes(context.Background(), map[string]string{metadataProviderKey: providerName, metadataEndpointKey: "endpoint"}); err != nil {
		t.Fatalf("ListSandboxes err=%v", err)
	}
	access, err := client.ActivateSandbox(context.Background(), "sb_123")
	if err != nil {
		t.Fatalf("ActivateSandbox err=%v", err)
	}
	if access.AccessToken != "ss_test_token" {
		t.Fatalf("access token=%q", access.AccessToken)
	}
	if _, err := client.UpdateSandboxMetadata(context.Background(), "sb_123", map[string]string{metadataProviderKey: providerName}); err != nil {
		t.Fatalf("UpdateSandboxMetadata err=%v", err)
	}
	if err := client.DeleteSandbox(context.Background(), "sb_123"); err != nil {
		t.Fatalf("DeleteSandbox err=%v", err)
	}
	for _, req := range requests {
		if req.key != "ss_test_key" {
			t.Fatalf("%s %s X-API-Key=%q", req.method, req.path, req.key)
		}
	}
}

func TestSuperserveClientRejectsCrossOriginRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("cross-origin redirect target should not be reached")
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/sandboxes", http.StatusFound)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("err=%v, want cross-origin redirect refusal", err)
	}
}

func TestSuperserveClientRedactsSecretsFromErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad ss_test_key token {\"access_token\": \"ss_test_token\"}"}`)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Probe(context.Background())
	if err == nil {
		t.Fatal("expected probe error")
	}
	if strings.Contains(err.Error(), "ss_test_key") || strings.Contains(err.Error(), "ss_test_token") {
		t.Fatalf("secret leaked in error: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error was not redacted: %v", err)
	}
}

func TestSuperserveClientUpdateDoesNotFabricateMissingMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/sandboxes/sb_123" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeTestJSON(w, map[string]any{"id": "sb_123"})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := client.UpdateSandboxMetadata(context.Background(), "sb_123", map[string]string{metadataClaimKey: leasePrefix + "sb_123"})
	if err != nil {
		t.Fatalf("UpdateSandboxMetadata err=%v", err)
	}
	if sb.Metadata != nil {
		t.Fatalf("metadata was fabricated from request: %#v", sb.Metadata)
	}
}

func TestSuperserveClientUpdateFetchesSandboxAfterEmptyPatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/sandboxes/sb_123":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/sandboxes/sb_123":
			writeTestJSON(w, map[string]any{"id": "sb_123", "metadata": map[string]string{metadataClaimKey: leasePrefix + "sb_123"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := client.UpdateSandboxMetadata(context.Background(), "sb_123", map[string]string{metadataClaimKey: leasePrefix + "sb_123"})
	if err != nil {
		t.Fatalf("UpdateSandboxMetadata err=%v", err)
	}
	if sb.Metadata[metadataClaimKey] != leasePrefix+"sb_123" {
		t.Fatalf("metadata was not fetched after empty patch: %#v", sb.Metadata)
	}
}

func TestSuperserveClientDataPlaneUploadAndStreamUseAccessTokenRouting(t *testing.T) {
	var requests []struct {
		method      string
		path        string
		query       string
		apiKey      string
		accessToken string
		sandboxID   string
		body        string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, struct {
			method      string
			path        string
			query       string
			apiKey      string
			accessToken string
			sandboxID   string
			body        string
		}{
			method:      r.Method,
			path:        r.URL.Path,
			query:       r.URL.RawQuery,
			apiKey:      r.Header.Get("X-API-Key"),
			accessToken: r.Header.Get("X-Access-Token"),
			sandboxID:   r.Header.Get("X-Superserve-Sandbox-Id"),
			body:        string(body),
		})
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/files":
			if r.URL.Query().Get("path") != "/tmp/crabbox-sync-test.tgz" {
				t.Fatalf("upload path query=%q", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/exec/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"stdout\":\"out\\n\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"stderr\":\"err\\n\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"finished\":true,\"exit_code\":3}\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	access := &sandboxAccess{Sandbox: superserveSandbox{ID: "sb_123"}, AccessToken: "ss_test_token"}
	if err := client.UploadFile(context.Background(), access, "/tmp/crabbox-sync-test.tgz", strings.NewReader("archive")); err != nil {
		t.Fatalf("UploadFile err=%v", err)
	}
	var stdout, stderr strings.Builder
	result, err := client.Exec(context.Background(), access, execRequest{
		Command:     "go test ./...",
		WorkingDir:  "/workspace/crabbox",
		Env:         map[string]string{"SUPERSERVE_API_KEY": "ss_test_not_real"},
		TimeoutSecs: 12,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Exec err=%v", err)
	}
	if result.ExitCode != 3 || stdout.String() != "out\n" || stderr.String() != "err\n" {
		t.Fatalf("result=%#v stdout=%q stderr=%q", result, stdout.String(), stderr.String())
	}
	if len(requests) != 2 {
		t.Fatalf("requests=%#v", requests)
	}
	for _, req := range requests {
		if req.apiKey != "" {
			t.Fatalf("data-plane request used API key: %#v", req)
		}
		if req.accessToken != "ss_test_token" || req.sandboxID != "sb_123" {
			t.Fatalf("data-plane auth/routing headers wrong: %#v", req)
		}
	}
	if !strings.Contains(requests[1].body, `"env":{"SUPERSERVE_API_KEY":"ss_test_not_real"}`) || strings.Contains(requests[1].body, "X-API-Key") {
		t.Fatalf("exec body=%q", requests[1].body)
	}
}

func TestSuperserveClientRefreshesAccessTokenOnDataPlane401(t *testing.T) {
	execAttempts := 0
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/exec/stream":
			execAttempts++
			tokens = append(tokens, r.Header.Get("X-Access-Token"))
			if execAttempts == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"expired token"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"stdout\":\"fresh\\n\"}\n\n")
			_, _ = io.WriteString(w, "data: {\"finished\":true,\"exit_code\":0}\n\n")
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/sb_123/activate":
			if r.Header.Get("X-API-Key") != "ss_test_key" {
				t.Fatalf("activate X-API-Key=%q", r.Header.Get("X-API-Key"))
			}
			writeTestJSON(w, map[string]any{"access_token": "ss_test_token_fresh", "sandbox": map[string]any{"id": "sb_123", "status": "running"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	access := &sandboxAccess{Sandbox: superserveSandbox{ID: "sb_123"}, AccessToken: "ss_test_token_old"}
	var stdout strings.Builder
	result, err := client.Exec(context.Background(), access, execRequest{Command: "true"}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("Exec err=%v", err)
	}
	if result.ExitCode != 0 || stdout.String() != "fresh\n" {
		t.Fatalf("result=%#v stdout=%q", result, stdout.String())
	}
	if strings.Join(tokens, ",") != "ss_test_token_old,ss_test_token_fresh" || access.AccessToken != "ss_test_token_fresh" {
		t.Fatalf("tokens=%#v access=%#v", tokens, access)
	}
}

func TestSuperserveClientFallsBackToBufferedExecWhenStreamUnsupported(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/exec/stream":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"missing"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/exec":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["command"] != "printf hi" || body["working_dir"] != "/workspace/crabbox" {
				t.Fatalf("body=%#v", body)
			}
			writeTestJSON(w, map[string]any{"stdout": "hi", "stderr": "warn", "exit_code": 4})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	result, err := client.Exec(context.Background(), &sandboxAccess{Sandbox: superserveSandbox{ID: "sb_123"}, AccessToken: "ss_test_token"},
		execRequest{Command: "printf hi", WorkingDir: "/workspace/crabbox"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Exec err=%v", err)
	}
	if strings.Join(paths, ",") != "/exec/stream,/exec" {
		t.Fatalf("paths=%#v", paths)
	}
	if result.ExitCode != 4 || stdout.String() != "hi" || stderr.String() != "warn" {
		t.Fatalf("result=%#v stdout=%q stderr=%q", result, stdout.String(), stderr.String())
	}
}

func TestSuperserveClientCustomControlPlaneUsesProductionDataPlane(t *testing.T) {
	client := &httpSuperserveClient{baseURL: "https://api.example.test"}
	target, err := client.dataPlaneTarget("sb_123")
	if err != nil {
		t.Fatal(err)
	}
	if target.baseURL != "https://sandbox.superserve.ai" || target.headers["X-Superserve-Sandbox-Id"] != "sb_123" {
		t.Fatalf("target=%#v, want official SDK production data-plane fallback", target)
	}
}

func TestSuperserveUploadHonorsCallerDeadline(t *testing.T) {
	var remaining time.Duration
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok {
			t.Fatal("upload request has no deadline")
		}
		remaining = time.Until(deadline)
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Status:     "204 No Content",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})}
	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL("http://localhost"), Runtime{HTTP: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := client.UploadFile(ctx, &sandboxAccess{Sandbox: superserveSandbox{ID: "sb_123"}, AccessToken: "ss_test_token"},
		"/tmp/archive.tgz", strings.NewReader("archive")); err != nil {
		t.Fatal(err)
	}
	if remaining < 9*time.Minute {
		t.Fatalf("upload deadline remaining=%s, caller deadline was shortened", remaining)
	}
}

func TestSuperserveExecRequestContextPreservesServiceDefault(t *testing.T) {
	ctx, cancel := superserveExecRequestContext(context.Background(), 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("service-default exec timeout added an absolute client deadline")
	}

	ctx, cancel = superserveExecRequestContext(context.Background(), 12)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("positive exec timeout did not add a client deadline")
	}
	remaining := time.Until(deadline)
	if remaining < 16*time.Second || remaining > 18*time.Second {
		t.Fatalf("positive exec timeout remaining=%s, want about 17s", remaining)
	}
}

func TestSuperserveClientStreamDoesNotRetainUnboundedOutput(t *testing.T) {
	chunk := strings.Repeat("x", maxExecStreamCaptureBytes+2048)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/exec/stream" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_ = json.NewEncoder(w).Encode(map[string]string{})
		_, _ = io.WriteString(w, "data: "+mustJSONLine(t, map[string]string{"stdout": chunk})+"\n\n")
		_, _ = io.WriteString(w, "data: {\"finished\":true,\"exit_code\":0}\n\n")
	}))
	defer server.Close()

	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	result, err := client.Exec(context.Background(), &sandboxAccess{Sandbox: superserveSandbox{ID: "sb_123"}, AccessToken: "ss_test_token"},
		execRequest{Command: "yes"}, &stdout, io.Discard)
	if err != nil {
		t.Fatalf("Exec err=%v", err)
	}
	if stdout.Len() != len(chunk) {
		t.Fatalf("stdout len=%d want %d", stdout.Len(), len(chunk))
	}
	if len(result.Stdout) != maxExecStreamCaptureBytes {
		t.Fatalf("captured stdout len=%d want %d", len(result.Stdout), maxExecStreamCaptureBytes)
	}
	if result.Stdout != chunk[len(chunk)-maxExecStreamCaptureBytes:] {
		t.Fatal("captured stdout is not the bounded tail")
	}
}

func TestSuperserveClientPropagatesOutputWriterFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		stream     bool
		failStdout bool
		want       string
	}{
		{name: "stream stdout", stream: true, failStdout: true, want: "write command stdout"},
		{name: "stream stderr", stream: true, want: "write command stderr"},
		{name: "buffered stdout", failStdout: true, want: "write command stdout"},
		{name: "buffered stderr", want: "write command stderr"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/exec/stream":
					if !test.stream {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"stdout\":\"out\",\"stderr\":\"err\"}\n\n")
					_, _ = io.WriteString(w, "data: {\"finished\":true,\"exit_code\":0}\n\n")
				case "/exec":
					writeTestJSON(w, map[string]any{"stdout": "out", "stderr": "err", "exit_code": 0})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
			client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			stdout, stderr := io.Writer(io.Discard), io.Writer(io.Discard)
			if test.failStdout {
				stdout = failingWriter{err: errors.New("stdout closed")}
			} else {
				stderr = failingWriter{err: errors.New("stderr closed")}
			}
			_, err = client.Exec(context.Background(), &sandboxAccess{Sandbox: superserveSandbox{ID: "sb_123"}, AccessToken: "ss_test_token"},
				execRequest{Command: "true"}, stdout, stderr)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Exec err=%v, want %q", err, test.want)
			}
		})
	}
}

func TestSuperserveClientRedactsForwardedEnvValuesFromExecErrors(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/exec/stream" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"bad env ss_test_not_real"}}`)
		}))
		defer server.Close()

		t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
		client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Exec(context.Background(), &sandboxAccess{Sandbox: superserveSandbox{ID: "sb_123"}, AccessToken: "ss_test_token"},
			execRequest{Command: "true", Env: map[string]string{"SECRET_TOKEN": "ss_test_not_real"}}, io.Discard, io.Discard)
		if err == nil {
			t.Fatal("expected exec error")
		}
		if strings.Contains(err.Error(), "ss_test_not_real") || !strings.Contains(err.Error(), "[redacted]") {
			t.Fatalf("error was not redacted: %v", err)
		}
	})

	t.Run("buffered fallback", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/exec/stream":
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"message":"stream unavailable"}}`)
			case "/exec":
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":{"message":"bad env ss_test_not_real"}}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
		client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Exec(context.Background(), &sandboxAccess{Sandbox: superserveSandbox{ID: "sb_123"}, AccessToken: "ss_test_token"},
			execRequest{Command: "true", Env: map[string]string{"SECRET_TOKEN": "ss_test_not_real"}}, io.Discard, io.Discard)
		if err == nil {
			t.Fatal("expected exec error")
		}
		if strings.Contains(err.Error(), "ss_test_not_real") || !strings.Contains(err.Error(), "[redacted]") {
			t.Fatalf("error was not redacted: %v", err)
		}
	})
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestSuperserveClientStreamRequiresFinishedEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/exec/stream" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"stdout\":\"partial\"}\n\n")
	}))
	defer server.Close()

	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Exec(context.Background(), &sandboxAccess{Sandbox: superserveSandbox{ID: "sb_123"}, AccessToken: "ss_test_token"},
		execRequest{Command: "long"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "without a finished event") {
		t.Fatalf("err=%v, want unfinished stream error", err)
	}
}

func TestSuperserveClientRejectsTerminalStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/exec/stream" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"finished\":true,\"error\":\"failed with project_test_not_real\"}\n\n")
	}))
	defer server.Close()

	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	result, err := client.Exec(context.Background(), &sandboxAccess{Sandbox: superserveSandbox{ID: "sb_123"}, AccessToken: "ss_test_token"},
		execRequest{Command: "false", Env: map[string]string{"PROJECT_TOKEN": "project_test_not_real"}}, io.Discard, &stderr)
	if err == nil || !strings.Contains(err.Error(), "command stream failed") {
		t.Fatalf("result=%#v err=%v, want terminal stream failure", result, err)
	}
	if strings.Contains(err.Error(), "project_test_not_real") || strings.Contains(stderr.String(), "project_test_not_real") {
		t.Fatalf("terminal stream error leaked env value: err=%v stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "[redacted]") {
		t.Fatalf("stderr=%q, want redacted terminal error", stderr.String())
	}
}

func TestSuperserveClientPreservesExitCodeWithTerminalStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/exec/stream" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"finished\":true,\"exit_code\":23,\"error\":\"command failed\"}\n\n")
	}))
	defer server.Close()

	t.Setenv("CRABBOX_SUPERSERVE_API_KEY", "ss_test_key")
	client, err := newSuperserveClient(testConfigWithBaseURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	result, err := client.Exec(context.Background(), &sandboxAccess{Sandbox: superserveSandbox{ID: "sb_123"}, AccessToken: "ss_test_token"},
		execRequest{Command: "false"}, io.Discard, &stderr)
	if err != nil {
		t.Fatalf("Exec err=%v", err)
	}
	if result.ExitCode != 23 || !strings.Contains(stderr.String(), "command failed") {
		t.Fatalf("result=%#v stderr=%q, want exit 23 and terminal error output", result, stderr.String())
	}
}

func mustJSONLine(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDataPlaneHostForSandbox(t *testing.T) {
	got := dataPlaneHostForSandbox("sb_123", "sandbox.example.test")
	if got != "boxd-sb_123.sandbox.example.test" {
		t.Fatalf("host=%q", got)
	}
	if dataPlaneHostForSandbox("", "sandbox.example.test") != "" {
		t.Fatal("empty sandbox id should not derive host")
	}
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func testConfigWithBaseURL(baseURL string) Config {
	cfg := testConfig()
	cfg.Superserve.BaseURL = baseURL
	return cfg
}
