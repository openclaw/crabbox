package crownest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientSandboxWorkspaceRunAndArchiveFlow(t *testing.T) {
	var requests []struct {
		method string
		path   string
		body   string
		auth   string
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, struct {
			method string
			path   string
			body   string
			auth   string
		}{r.Method, r.URL.Path, string(body), r.Header.Get("Authorization")})
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			writeJSON(w, map[string]any{"sandbox": map[string]any{"id": "sbx_123", "status": "running"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sbx_123":
			writeJSON(w, map[string]any{"sandbox": map[string]any{"id": "sbx_123", "status": "running"}})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/sbx_123":
			writeJSON(w, map[string]any{"sandbox": map[string]any{"id": "sbx_123", "status": "destroyed"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/workspace-runs":
			writeJSON(w, map[string]any{"workspaceRun": map[string]any{"id": "wsr_123", "status": "awaiting_archive", "sandboxId": "sbx_123"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/workspace-runs/wsr_123/archive-transfer":
			writeJSON(w, map[string]any{"transfer": map[string]any{"id": "upl_123", "method": "PUT", "uploadUrl": server.URL + "/v1/workspace-runs/wsr_123/archive-transfer/upl_123", "maxSizeBytes": 1024, "headers": map[string]string{"x-test": "1"}}})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/workspace-runs/wsr_123/archive-transfer/upl_123":
			if r.Header.Get("x-test") != "1" {
				t.Fatalf("upload header x-test=%q", r.Header.Get("x-test"))
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/workspace-runs/wsr_123/archive/finalize":
			writeJSON(w, map[string]any{"workspaceRun": map[string]any{"id": "wsr_123", "status": "archive_uploaded"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/workspace-runs/wsr_123/start":
			writeJSON(w, map[string]any{"workspaceRun": map[string]any{"id": "wsr_123", "status": "running", "sandboxId": "sbx_123"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/workspace-runs/wsr_123/cancel":
			writeJSON(w, map[string]any{"workspaceRun": map[string]any{"id": "wsr_123", "status": "canceled"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	t.Setenv("CRABBOX_CROWNEST_API_KEY", "cn_test_key")
	client, err := newClient(testConfigWithURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateSandbox(context.Background(), createSandboxRequest{Template: "python-node"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetSandbox(context.Background(), "sbx_123"); err != nil {
		t.Fatal(err)
	}
	run, err := client.CreateWorkspaceRun(context.Background(), createWorkspaceRunRequest{Command: "pnpm test"}, "create")
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := client.CreateArchiveTransfer(context.Background(), run.ID, createArchiveTransferRequest{SHA256: strings.Repeat("a", 64), SizeBytes: 7}, "transfer")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.UploadArchive(context.Background(), transfer, strings.NewReader("archive")); err != nil {
		t.Fatal(err)
	}
	if _, err := client.FinalizeArchive(context.Background(), run.ID, finalizeArchiveRequest{SHA256: strings.Repeat("a", 64), SizeBytes: 7, UploadID: transfer.ID}, "finalize"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartWorkspaceRun(context.Background(), run.ID, "start"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CancelWorkspaceRun(context.Background(), run.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteSandbox(context.Background(), "sbx_123"); err != nil {
		t.Fatal(err)
	}
	for _, req := range requests {
		if req.auth != "Bearer cn_test_key" {
			t.Fatalf("%s %s Authorization=%q", req.method, req.path, req.auth)
		}
		if strings.Contains(req.body, "cn_test_key") {
			t.Fatalf("secret leaked in body: %s", req.body)
		}
	}
}

func TestReadSSEParsesEvents(t *testing.T) {
	var got []streamEvent
	input := strings.Join([]string{
		"event: stdout",
		`data: {"type":"stdout","seq":1,"data":"ok\n"}`,
		"",
		`data: {"type":"terminal","seq":2,"workspaceRun":{"id":"wsr_123","status":"succeeded","exitCode":0}}`,
		"",
	}, "\r\n")
	if err := readSSE(strings.NewReader(input), func(event streamEvent) error {
		got = append(got, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Type != "stdout" || got[1].WorkspaceRun.ID != "wsr_123" {
		t.Fatalf("events=%#v", got)
	}
}

func TestClientRedactsSecretsFromErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad cn_test_key"}}`)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_CROWNEST_API_KEY", "cn_test_key")
	client, err := newClient(testConfigWithURL(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Probe(context.Background())
	if err == nil {
		t.Fatal("expected probe error")
	}
	if strings.Contains(err.Error(), "cn_test_key") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error was not redacted: %v", err)
	}
}

func TestUploadArchiveRedactsPresignedURLFromTransportError(t *testing.T) {
	const signedQuery = "X-Amz-Credential=secret-credential&X-Amz-Signature=secret-signature"
	client := &httpClient{
		baseURL: "https://api.crownest.dev",
		apiKey:  "cn_test_key",
		http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial %s: connection refused", req.URL.String())
		})},
	}

	err := client.UploadArchive(context.Background(), archiveTransfer{
		Method:    http.MethodPut,
		UploadURL: "https://upload.example.test/archive.tgz?" + signedQuery,
	}, strings.NewReader("archive"))
	if err == nil {
		t.Fatal("expected upload error")
	}
	message := err.Error()
	if strings.Contains(message, "secret-credential") || strings.Contains(message, "secret-signature") || strings.Contains(message, signedQuery) {
		t.Fatalf("upload error leaked signed query: %v", err)
	}
	if !strings.Contains(message, "https://upload.example.test/archive.tgz?[redacted]") {
		t.Fatalf("upload error omitted redacted URL marker: %v", err)
	}
}

func TestClientKeepsRequestContextAliveUntilJSONBodyRead(t *testing.T) {
	client := &httpClient{
		baseURL: "https://api.crownest.dev",
		apiKey:  "cn_test_key",
		http: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				Body:       &contextCheckingBody{ctx: req.Context(), data: []byte(`{"sandbox":{"id":"sbx_123"}}`)},
				Header:     make(http.Header),
				Request:    req,
				StatusCode: http.StatusOK,
			}, nil
		})},
	}
	if _, err := client.GetSandbox(context.Background(), "sbx_123"); err != nil {
		t.Fatal(err)
	}
}

func testConfigWithURL(url string) Config {
	cfg := testConfig()
	cfg.Crownest.APIURL = url
	return cfg
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type contextCheckingBody struct {
	ctx  context.Context
	data []byte
	done bool
}

func (b *contextCheckingBody) Read(p []byte) (int, error) {
	if b.done {
		return 0, io.EOF
	}
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	default:
	}
	b.done = true
	return copy(p, b.data), nil
}

func (b *contextCheckingBody) Close() error { return nil }
