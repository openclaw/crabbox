package blaxel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestRedactErrorPreservesCauseAndSafeFormatting(t *testing.T) {
	if err := redactError(nil); err != nil {
		t.Fatalf("redactError(nil)=%v", err)
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("fixture failure")} {
		t.Run(cause.Error(), func(t *testing.T) {
			t.Setenv("CRABBOX_BLAXEL_API_KEY", "synthetic-first-key")
			t.Setenv("BL_API_KEY", "synthetic-second-key")
			original := &url.Error{Op: "Get", URL: "https://example.test/synthetic-first-key/synthetic-second-key", Err: cause}
			want := redactString(original.Error())
			err := redactError(original)
			t.Setenv("CRABBOX_BLAXEL_API_KEY", "")
			t.Setenv("BL_API_KEY", "")
			if !errors.Is(err, cause) {
				t.Errorf("redacted error lost cause %v", cause)
			}
			var urlErr *url.Error
			if !errors.As(err, &urlErr) || urlErr != original {
				t.Error("redacted error lost typed transport cause")
			}
			var exitErr core.ExitError
			if errors.As(err, &exitErr) {
				t.Error("client error invented a CLI exit code")
			}
			for _, text := range []string{err.Error(), fmt.Sprint(err), fmt.Sprintf("%+v", err), fmt.Sprintf("%s", err)} {
				if text != want {
					t.Errorf("display=%q, want frozen redacted message %q", text, want)
				}
			}
			if got := fmt.Errorf("outer: %w", err).Error(); got != "outer: "+want {
				t.Errorf("wrapped display=%q", got)
			}
		})
	}
}

func TestRedactedNativeHTTPRequestPreservesCancellation(t *testing.T) {
	for _, multipart := range []bool{false, true} {
		t.Run(fmt.Sprintf("multipart=%t", multipart), func(t *testing.T) {
			t.Setenv("CRABBOX_BLAXEL_API_KEY", "synthetic-query-key")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				cancel()
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
					t.Error("canceled HTTP request did not close")
				}
			}))
			defer server.Close()
			client := &restClient{http: server.Client(), dataHTTP: server.Client()}
			values := url.Values{"fixture": {"synthetic-query-key"}}
			var err error
			if multipart {
				_, err = client.doMultipartAt(ctx, server.URL, http.MethodPut, "/fixture", values, "application/octet-stream", strings.NewReader("fixture"), nil)
			} else {
				_, err = client.doAt(ctx, client.http, server.URL, http.MethodGet, "/fixture", values, nil, nil)
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("native HTTP error %v lost cancellation", err)
			}
			if err == nil || strings.Contains(fmt.Sprintf("%+v", err), "synthetic-query-key") || !strings.Contains(err.Error(), "<redacted>") {
				t.Errorf("native error was not safely redacted: %v", err)
			}
		})
	}
}

func TestWaitProcessStopsAfterNativeHTTPCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stops atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/sandboxes/sbx-owned":
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{
				"name": "sbx-owned", "url": serverURL(r) + "/sandbox/sbx-owned",
			}, "status": "DEPLOYED"})
		case r.Method == http.MethodGet && r.URL.Path == "/sandbox/sbx-owned/process/proc-owned":
			cancel()
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
				t.Error("canceled process request did not close")
			}
		case r.Method == http.MethodDelete && r.URL.Path == "/sandbox/sbx-owned/process/proc-owned":
			stops.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{APIURL: server.URL, APIKey: "synthetic-key"}}, core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	b := &backend{}
	_, err = b.waitProcess(ctx, client, "sbx-owned", Process{ID: "proc-owned", Status: "running"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("waitProcess error %v lost cancellation", err)
	}
	if got := stops.Load(); got != 1 {
		t.Errorf("process stops=%d, want exactly the original process stopped", got)
	}
}

func TestUploadFileRewindsArchiveAfterNativeMultipartFailure(t *testing.T) {
	var attempts atomic.Int32
	parts := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/sandboxes/sbx-owned":
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "sbx-owned", "url": serverURL(r) + "/sandbox/sbx-owned"}, "status": "DEPLOYED"})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/filesystem-multipart/initiate/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"uploadId": fmt.Sprintf("upload-%d", attempts.Add(1))})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/part"):
			if err := r.ParseMultipartForm(1024); err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			defer r.MultipartForm.RemoveAll()
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			select {
			case parts <- string(data):
			default:
				t.Error("unexpected extra multipart attempt")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if strings.Contains(r.URL.Path, "upload-1/") {
				http.Error(w, "WORKLOAD_UNAVAILABLE", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"etag": "synthetic-etag", "partNumber": 1, "size": len(data)})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{APIURL: server.URL, APIKey: "synthetic-key"}}, core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "archive.tgz")
	want := "archive-bytes\x00with-tail"
	if err := os.WriteFile(archive, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := client.UploadFile(ctx, "sbx-owned", "/tmp/archive.tgz", file); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || len(parts) != 2 {
		t.Fatalf("attempts=%d parts=%d", attempts.Load(), len(parts))
	}
	for i := 0; i < 2; i++ {
		if got := <-parts; got != want {
			t.Fatalf("attempt %d body=%q want complete archive", i+1, got)
		}
	}
}

func TestBlaxelFallbackBoundsControlAndPreservesUpload(t *testing.T) {
	const controlTimeout = 30 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/sandboxes":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `[{"metadata":`)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.Method == http.MethodGet && r.URL.Path == "/v0/sandboxes/sbx-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{
				"name": "sbx-1",
				"url":  serverURL(r) + "/sandbox/sbx-1",
			}})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/filesystem-multipart/initiate/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"uploadId": "upload-1"})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/filesystem-multipart/upload-1/part"):
			if err := r.ParseMultipartForm(1024); err != nil {
				t.Error(err)
			}
			time.Sleep(3 * controlTimeout)
			_ = json.NewEncoder(w).Encode(map[string]any{"etag": "etag-1", "partNumber": 1})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/filesystem-multipart/upload-1/complete"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	control, data := blaxelHTTPClients(nil, controlTimeout)
	client := &restClient{
		base:     server.URL,
		apiKey:   "test-key",
		version:  defaultAPIVersion,
		http:     secureHTTPClient(control),
		dataHTTP: secureHTTPClient(data),
	}
	started := time.Now()
	err := client.Probe(context.Background())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Probe error=%v, want whole-request deadline", err)
	}
	controlElapsed := time.Since(started)
	if controlElapsed >= time.Second {
		t.Fatalf("stalled control response bounded after %s, want under 1s", controlElapsed)
	}

	started = time.Now()
	if err := client.UploadFile(context.Background(), "sbx-1", "/tmp/archive.tgz", strings.NewReader("archive")); err != nil {
		t.Fatal(err)
	}
	dataElapsed := time.Since(started)
	if dataElapsed <= controlTimeout {
		t.Fatalf("upload completed in %s, want beyond %s", dataElapsed, controlTimeout)
	}
	t.Logf("Blaxel control body bounded in %s; multipart upload completed in %s beyond %s control deadline", controlElapsed.Round(time.Millisecond), dataElapsed.Round(time.Millisecond), controlTimeout)
}

func TestBlaxelInjectedHTTPSettingsArePreservedForBothPlanes(t *testing.T) {
	transport := &http.Transport{DisableKeepAlives: true}
	injected := &http.Client{Transport: transport, Timeout: 17 * time.Second}
	api, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{APIKey: "test-key"}}, core.Runtime{HTTP: injected})
	if err != nil {
		t.Fatal(err)
	}
	client := api.(*restClient)
	if client.http.Transport != transport || client.dataHTTP.Transport != transport || client.http.Timeout != injected.Timeout || client.dataHTTP.Timeout != injected.Timeout {
		t.Fatalf("settings=control:(%T,%s) data:(%T,%s)", client.http.Transport, client.http.Timeout, client.dataHTTP.Transport, client.dataHTTP.Timeout)
	}
	if injected.CheckRedirect != nil {
		t.Fatal("constructor mutated injected redirect policy")
	}
}

func TestBlaxelControlRedirectDoesNotLeakCredentials(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization=%q", got)
		}
		http.Redirect(w, r, target.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	api, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{APIURL: origin.URL, APIKey: "test-key"}}, core.Runtime{HTTP: origin.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("Probe error=%v, want redirect rejection", err)
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("redirect target requests=%d, want 0", targetRequests.Load())
	}
}

func TestValidateAPIURLCanonicalizesAndRejectsUnsafe(t *testing.T) {
	got, err := ValidateAPIURL("https://API.BLAXEL.AI:443/v1/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://api.blaxel.ai" {
		t.Fatalf("ValidateAPIURL canonical=%q", got)
	}
	if got, err := ValidateAPIURL("http://localhost:8080/v1"); err != nil || got != "http://localhost:8080" {
		t.Fatalf("loopback ValidateAPIURL=%q err=%v", got, err)
	}
	for _, raw := range []string{
		"https://user:pass@api.blaxel.ai",
		"https://api.blaxel.ai?api_key=secret",
		"https://api.blaxel.ai#secret",
		"http://api.blaxel.ai",
	} {
		if _, err := ValidateAPIURL(raw); err == nil {
			t.Fatalf("ValidateAPIURL(%q) succeeded", raw)
		}
	}
}

func TestValidateSandboxEndpointRestrictsBearerTokenDestinations(t *testing.T) {
	got, err := validateSandboxEndpoint("https://SBX-ONE-WORKSPACE.us-pdx-1.BL.RUN:443/api/", "https://api.blaxel.ai")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://sbx-one-workspace.us-pdx-1.bl.run/api" {
		t.Fatalf("endpoint=%q", got)
	}
	if got, err := validateSandboxEndpoint("http://127.0.0.1:8080/sandbox/sbx-1", "http://localhost:9999"); err != nil || got != "http://127.0.0.1:8080/sandbox/sbx-1" {
		t.Fatalf("loopback endpoint=%q err=%v", got, err)
	}
	for _, tc := range []struct {
		name       string
		endpoint   string
		management string
	}{
		{name: "userinfo", endpoint: "https://user:pass@sbx-one.us-pdx-1.bl.run", management: "https://api.blaxel.ai"},
		{name: "query", endpoint: "https://sbx-one.us-pdx-1.bl.run?token=secret", management: "https://api.blaxel.ai"},
		{name: "fragment", endpoint: "https://sbx-one.us-pdx-1.bl.run#secret", management: "https://api.blaxel.ai"},
		{name: "http remote", endpoint: "http://sbx-one.us-pdx-1.bl.run", management: "https://api.blaxel.ai"},
		{name: "untrusted host", endpoint: "https://evil.example/sandbox", management: "https://api.blaxel.ai"},
		{name: "loopback with remote management", endpoint: "http://127.0.0.1:8080/sandbox", management: "https://api.blaxel.ai"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateSandboxEndpoint(tc.endpoint, tc.management); err == nil {
				t.Fatalf("validateSandboxEndpoint(%q, %q) succeeded", tc.endpoint, tc.management)
			}
		})
	}
}

func TestClientHeadersAndListShapes(t *testing.T) {
	var sawWorkspace, sawVersion, sawAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/sandboxes" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		sawWorkspace = r.Header.Get("X-Blaxel-Workspace") == "workspace-test"
		sawVersion = r.Header.Get("Blaxel-Version") == defaultAPIVersion
		sawAuth = r.Header.Get("Authorization") == "Bearer test-key"
		if r.URL.Query().Get("limit") != "2" {
			t.Fatalf("query=%s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("q") != "workspace-token" {
			t.Fatalf("query=%s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"metadata": map[string]any{
					"name":   "sbx-1",
					"url":    serverURL(r) + "/sandbox/sbx-1",
					"labels": map[string]string{"env": "dev"},
				},
				"spec":   map[string]any{"region": "us-pdx-1", "runtime": map[string]any{"image": "ubuntu:24.04"}},
				"status": "DEPLOYED",
			}},
			"meta": map[string]any{"nextCursor": "cursor-2"},
		})
	}))
	defer server.Close()

	client, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{
		APIURL:    server.URL,
		APIKey:    "test-key",
		Workspace: "workspace-test",
	}}, core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ListSandboxes(context.Background(), ListSandboxesRequest{
		Limit:  2,
		Labels: map[string]string{"crabbox.blaxel.scope": "workspace-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawWorkspace || !sawVersion || !sawAuth {
		t.Fatalf("headers workspace=%t version=%t auth=%t", sawWorkspace, sawVersion, sawAuth)
	}
	if len(result.Sandboxes) != 1 ||
		result.Sandboxes[0].ID != "sbx-1" ||
		result.Sandboxes[0].Endpoint == "" ||
		result.Sandboxes[0].Labels["env"] != "dev" ||
		result.Next != "cursor-2" {
		t.Fatalf("result=%#v", result)
	}

	bare, err := parseSandboxList([]byte(`[{"metadata":{"name":"sbx-2","url":"https://sbx.example","labels":{"team":"core"}},"state":"RUNNING"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(bare.Sandboxes) != 1 || bare.Sandboxes[0].ID != "sbx-2" || bare.Sandboxes[0].Status != "RUNNING" {
		t.Fatalf("bare=%#v", bare)
	}
}

func TestClientUsesManagementShapeAndSandboxDataPlane(t *testing.T) {
	var sawPutLabels bool
	var sawProcess bool
	var sawUpload bool
	var sawComplete bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/sandboxes/sbx-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{
					"name": "sbx-1",
					"url":  "http://" + r.Host + "/sandbox/sbx-1",
				},
				"spec":   map[string]any{"region": "us-pdx-1", "runtime": map[string]any{"image": "ubuntu:24.04"}},
				"status": "DEPLOYED",
			})
		case r.Method == http.MethodPut && r.URL.Path == "/v0/sandboxes/sbx-1":
			sawPutLabels = true
			var body struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Metadata.Labels["crabbox.lease"] != "blx_sbx-1" {
				t.Fatalf("put labels=%#v", body.Metadata.Labels)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"name": "sbx-1", "url": "http://" + r.Host + "/sandbox/sbx-1", "labels": body.Metadata.Labels},
				"status":   "DEPLOYED",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/sandbox/sbx-1/process":
			sawProcess = true
			var body struct {
				Command           string `json:"command"`
				WaitForCompletion bool   `json:"waitForCompletion"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Command != "'go' 'test' './...'" || body.WaitForCompletion {
				t.Fatalf("process body=%#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pid": "1234", "status": "running"})
		case r.Method == http.MethodPost && r.URL.Path == "/sandbox/sbx-1/filesystem-multipart/initiate//tmp/archive.tgz":
			_ = json.NewEncoder(w).Encode(map[string]any{"uploadId": "upload-1", "path": "/tmp/archive.tgz"})
		case r.Method == http.MethodPut && r.URL.Path == "/sandbox/sbx-1/filesystem-multipart/upload-1/part":
			sawUpload = true
			if r.Header.Get("Blaxel-Version") != defaultAPIVersion || r.Header.Get("X-Blaxel-Workspace") != "workspace-test" {
				t.Fatalf("multipart headers version=%q workspace=%q", r.Header.Get("Blaxel-Version"), r.Header.Get("X-Blaxel-Workspace"))
			}
			if r.URL.Query().Get("partNumber") != "1" {
				t.Fatalf("query=%s", r.URL.RawQuery)
			}
			if err := r.ParseMultipartForm(1024); err != nil {
				t.Fatal(err)
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "\x00\x01archive" {
				t.Fatalf("multipart upload=%q", data)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"etag": "etag-1", "partNumber": 1, "size": len(data)})
		case r.Method == http.MethodPost && r.URL.Path == "/sandbox/sbx-1/filesystem-multipart/upload-1/complete":
			sawComplete = true
			var body struct {
				Parts []multipartUploadPart `json:"parts"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Parts) != 1 || body.Parts[0].ETag != "etag-1" {
				t.Fatalf("complete body=%#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "ok"})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{
		APIURL:    server.URL,
		APIKey:    "test-key",
		Workspace: "workspace-test",
	}}, core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.UpdateSandboxLabels(context.Background(), "sbx-1", map[string]string{"crabbox.lease": "blx_sbx-1"}); err != nil {
		t.Fatal(err)
	}
	process, err := client.ExecuteProcess(context.Background(), "sbx-1", ExecuteProcessRequest{
		Command: "go",
		Args:    []string{"test", "./..."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if process.ID != "1234" || process.Status != "running" {
		t.Fatalf("process=%#v", process)
	}
	if err := client.UploadFile(context.Background(), "sbx-1", "/tmp/archive.tgz", strings.NewReader("\x00\x01archive")); err != nil {
		t.Fatal(err)
	}
	if !sawPutLabels || !sawProcess || !sawUpload || !sawComplete {
		t.Fatalf("sawPutLabels=%t sawProcess=%t sawUpload=%t sawComplete=%t", sawPutLabels, sawProcess, sawUpload, sawComplete)
	}
}

func TestClientRejectsUnsafeSandboxEndpointBeforeDataPlaneAuth(t *testing.T) {
	var dataPlaneRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/sandboxes/sbx-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{
					"name": "sbx-1",
					"url":  "https://evil.example/sandbox/sbx-1",
				},
				"status": "DEPLOYED",
			})
		default:
			dataPlaneRequests++
			t.Fatalf("unexpected data-plane request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{
		APIURL: server.URL,
		APIKey: "test-key",
	}}, core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ExecuteProcess(context.Background(), "sbx-1", ExecuteProcessRequest{Command: "true"})
	if err == nil || !strings.Contains(err.Error(), "not a trusted Blaxel data-plane origin") {
		t.Fatalf("ExecuteProcess err=%v", err)
	}
	if dataPlaneRequests != 0 {
		t.Fatalf("dataPlaneRequests=%d", dataPlaneRequests)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}

func TestClientRedactsAPIKeyFromErrors(t *testing.T) {
	t.Setenv("CRABBOX_BLAXEL_API_KEY", "secret-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad secret-key", http.StatusUnauthorized)
	}))
	defer server.Close()
	client, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{
		APIURL: server.URL,
		APIKey: "secret-key",
	}}, core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListSandboxes(context.Background(), ListSandboxesRequest{})
	if err == nil {
		t.Fatal("ListSandboxes succeeded")
	}
	if strings.Contains(err.Error(), "secret-key") || !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("error not redacted: %v", err)
	}
}

func TestSecureHTTPClientRedirectPolicy(t *testing.T) {
	client := secureHTTPClient(&http.Client{})
	req := &http.Request{URL: mustParseURL(t, "https://api.blaxel.ai/next")}
	var via []*http.Request
	for i := 0; i < 10; i++ {
		via = append(via, &http.Request{URL: mustParseURL(t, "https://api.blaxel.ai/loop")})
	}
	if err := client.CheckRedirect(req, via); err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("redirect cap err=%v", err)
	}
	crossOriginVia := []*http.Request{{URL: mustParseURL(t, "https://api.blaxel.ai/start")}}
	crossOriginReq := &http.Request{URL: mustParseURL(t, "https://evil.example/next")}
	if err := client.CheckRedirect(crossOriginReq, crossOriginVia); err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("cross-origin err=%v", err)
	}
	originalErr := errors.New("original policy")
	withOriginal := secureHTTPClient(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return originalErr
	}})
	if err := withOriginal.CheckRedirect(req, crossOriginVia); !errors.Is(err, originalErr) {
		t.Fatalf("original policy err=%v", err)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestAPICreateSandboxRequestMapsLifecyclePolicies(t *testing.T) {
	t.Parallel()

	got := apiCreateSandboxRequest(CreateSandboxRequest{
		Name:     "sbx-test",
		Image:    "blaxel/base-image:latest",
		Region:   "us-was-1",
		MemoryMB: 2048,
		TTL:      "20m",
		IdleTTL:  "10m",
	})

	if got.Spec.Runtime.TTL != "20m" {
		t.Fatalf("runtime.ttl=%q", got.Spec.Runtime.TTL)
	}
	if got.Spec.Lifecycle.TerminatedRetention != defaultTerminatedRetention {
		t.Fatalf("terminatedRetention=%q", got.Spec.Lifecycle.TerminatedRetention)
	}
	if len(got.Spec.Lifecycle.ExpirationPolicies) != 2 {
		t.Fatalf("policies=%#v", got.Spec.Lifecycle.ExpirationPolicies)
	}
	if got.Spec.Lifecycle.ExpirationPolicies[0] != (blaxelExpirationPolicy{Action: "delete", Type: "ttl-max-age", Value: "20m"}) {
		t.Fatalf("max-age policy=%#v", got.Spec.Lifecycle.ExpirationPolicies[0])
	}
	if got.Spec.Lifecycle.ExpirationPolicies[1] != (blaxelExpirationPolicy{Action: "delete", Type: "ttl-idle", Value: "10m"}) {
		t.Fatalf("idle policy=%#v", got.Spec.Lifecycle.ExpirationPolicies[1])
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		`"expirationPolicies"`,
		`"ttl-max-age"`,
		`"ttl-idle"`,
		`"terminatedRetention":"5m"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("create body missing %s: %s", want, body)
		}
	}
}

func TestAPICreateSandboxRequestDefaultsLifecycleWhenTTLUnset(t *testing.T) {
	t.Parallel()

	got := apiCreateSandboxRequest(CreateSandboxRequest{
		Name:  "sbx-default",
		Image: "blaxel/base-image:latest",
	})
	if len(got.Spec.Lifecycle.ExpirationPolicies) != 1 {
		t.Fatalf("policies=%#v", got.Spec.Lifecycle.ExpirationPolicies)
	}
	if got.Spec.Lifecycle.ExpirationPolicies[0].Type != "ttl-max-age" || got.Spec.Lifecycle.ExpirationPolicies[0].Value != defaultSandboxMaxAgeTTL {
		t.Fatalf("default policy=%#v", got.Spec.Lifecycle.ExpirationPolicies[0])
	}
	if got.Spec.Lifecycle.TerminatedRetention != defaultTerminatedRetention {
		t.Fatalf("terminatedRetention=%q", got.Spec.Lifecycle.TerminatedRetention)
	}
	if got.Spec.Runtime.TTL != defaultSandboxMaxAgeTTL {
		t.Fatalf("runtime.ttl=%q", got.Spec.Runtime.TTL)
	}
}

func TestAPICreateSandboxRequestHonorsExplicitTerminatedRetention(t *testing.T) {
	t.Parallel()

	got := apiCreateSandboxRequest(CreateSandboxRequest{
		Name:                "sbx-ret",
		IdleTTL:             "3m",
		TerminatedRetention: "15m",
	})
	if got.Spec.Lifecycle.TerminatedRetention != "15m" {
		t.Fatalf("terminatedRetention=%q", got.Spec.Lifecycle.TerminatedRetention)
	}
	if len(got.Spec.Lifecycle.ExpirationPolicies) != 1 || got.Spec.Lifecycle.ExpirationPolicies[0].Type != "ttl-idle" {
		t.Fatalf("policies=%#v", got.Spec.Lifecycle.ExpirationPolicies)
	}
}

func TestUpdateSandboxLabelsRoundTripsFullDocument(t *testing.T) {
	// Blaxel PUT is a full replace: the update body must carry the current
	// spec (runtime + lifecycle) or the sandbox gets DEACTIVATED.
	var putBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v0/sandboxes/sbx-1" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"name": "sbx-1", "labels": map[string]string{"old": "1"}},
				"spec": map[string]any{
					"region":  "us-pdx-1",
					"runtime": map[string]any{"image": "img", "memory": 4096},
					"lifecycle": map[string]any{
						"expirationPolicies":  []map[string]string{{"type": "ttl-max-age", "value": "10m", "action": "delete"}},
						"terminatedRetention": "1m",
					},
				},
				"status": "DEPLOYED",
			})
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/v0/sandboxes/sbx-1" {
			buf := new(strings.Builder)
			_, _ = io.Copy(buf, r.Body)
			putBody = buf.String()
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "sbx-1"}, "status": "DEPLOYED"})
			return
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)

	client, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{
		APIURL:    srv.URL,
		APIKey:    "test-key",
		Workspace: "workspace-test",
	}}, core.Runtime{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.UpdateSandboxLabels(context.Background(), "sbx-1", map[string]string{"crabbox.lease": "blx_sbx-1"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"crabbox.lease":"blx_sbx-1"`, `"lifecycle"`, `"ttl-max-age"`, `"terminatedRetention":"1m"`, `"runtime"`, `"region"`} {
		if !strings.Contains(putBody, want) {
			t.Fatalf("PUT body missing %s: %s", want, putBody)
		}
	}
	if strings.Contains(putBody, `"old":"1"`) {
		t.Fatalf("PUT body should replace labels, got %s", putBody)
	}
}

func TestDoSandboxRetryRecoversFromWorkloadUnavailable(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Management-plane sandbox lookup for sandboxBaseURL.
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v0/sandboxes/") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"name": "sbx", "url": "http://" + r.Host + "/sandbox/sbx"},
				"status":   "DEPLOYED",
			})
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/process") {
			calls++
			if calls < 3 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"WORKLOAD_UNAVAILABLE","status":404}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"pid":"123","status":"running"}`))
			return
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)

	client, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{
		APIURL:    srv.URL,
		APIKey:    "test-key",
		Workspace: "workspace-test",
	}}, core.Runtime{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	rc, ok := client.(*restClient)
	if !ok {
		t.Fatalf("unexpected client type %T", client)
	}

	oldDelays := blaxelWorkloadRetryDelays
	blaxelWorkloadRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { blaxelWorkloadRetryDelays = oldDelays })

	var out blaxelAPIProcess
	_, err = rc.doSandboxRetry(context.Background(), "sbx", http.MethodPost, "/process", nil, map[string]any{}, &out)
	if err != nil {
		t.Fatalf("doSandboxRetry: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
	if out.PID != "123" {
		t.Fatalf("unexpected pid %s", out.PID)
	}
}

func TestDoSandboxRetryDoesNotRetryOtherErrors(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v0/sandboxes/") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"name": "sbx", "url": "http://" + r.Host + "/sandbox/sbx"},
				"status":   "DEPLOYED",
			})
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/process") {
			calls++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad request"}`))
			return
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)

	client, err := newBlaxelClient(core.Config{Blaxel: core.BlaxelConfig{
		APIURL:    srv.URL,
		APIKey:    "test-key",
		Workspace: "workspace-test",
	}}, core.Runtime{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	rc, ok := client.(*restClient)
	if !ok {
		t.Fatalf("unexpected client type %T", client)
	}

	oldDelays := blaxelWorkloadRetryDelays
	blaxelWorkloadRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { blaxelWorkloadRetryDelays = oldDelays })

	var out blaxelAPIProcess
	_, err = rc.doSandboxRetry(context.Background(), "sbx", http.MethodPost, "/process", nil, map[string]any{}, &out)
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("expected 1 call for non-retryable error, got %d", calls)
	}
}
