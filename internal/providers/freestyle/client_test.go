package freestyle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFreestyleFileOperationsUseFilesPathPrefix(t *testing.T) {
	const (
		writePath = "/v1/vms/vm123/files/%2Ftmp%2Fcrabbox%20upload.tgz"
		readPath  = "/v1/vms/vm123/files/%2Fworkspace%2Frepo%2Ffile.txt"
	)
	var sawWrite, sawRead bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer test-key"; got != want {
			t.Errorf("authorization header=%q want %q", got, want)
		}
		switch r.Method {
		case http.MethodPut:
			sawWrite = true
			if got := r.URL.EscapedPath(); got != writePath {
				t.Errorf("write path=%q want %q", got, writePath)
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			sawRead = true
			if got := r.URL.EscapedPath(); got != readPath {
				t.Errorf("read path=%q want %q", got, readPath)
			}
			if err := json.NewEncoder(w).Encode(freestyleReadFileResponse{Content: "ok"}); err != nil {
				t.Errorf("encode response: %v", err)
			}
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client := &freestyleHTTPClient{
		apiKey:     "test-key",
		apiURL:     server.URL,
		httpClient: server.Client(),
	}
	if err := client.WriteFile(context.Background(), "vm123", "/tmp/crabbox upload.tgz", "payload", "base64"); err != nil {
		t.Fatalf("WriteFile err=%v", err)
	}
	got, err := client.ReadFile(context.Background(), "vm123", "workspace/repo/file.txt")
	if err != nil {
		t.Fatalf("ReadFile err=%v", err)
	}
	if got != "ok" {
		t.Fatalf("ReadFile content=%q", got)
	}
	if !sawWrite || !sawRead {
		t.Fatalf("sawWrite=%v sawRead=%v", sawWrite, sawRead)
	}
}

func TestFreestyleCreateVMOmitsUnsetSizing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Method, http.MethodPost; got != want {
			t.Errorf("method=%s want %s", got, want)
		}
		if got, want := r.URL.EscapedPath(), "/v1/vms"; got != want {
			t.Errorf("path=%q want %q", got, want)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if _, ok := body["vcpuCount"]; ok {
			t.Fatalf("vcpuCount should be omitted from default create request: %#v", body)
		}
		if _, ok := body["memSizeGb"]; ok {
			t.Fatalf("memSizeGb should be omitted from default create request: %#v", body)
		}
		if err := json.NewEncoder(w).Encode(freestyleCreateVMResponse{ID: "vm123"}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client := &freestyleHTTPClient{
		apiKey:     "test-key",
		apiURL:     server.URL,
		httpClient: server.Client(),
	}
	vm, err := client.CreateVM(context.Background(), freestyleCreateVMRequest{Name: "crabbox-test"})
	if err != nil {
		t.Fatalf("CreateVM err=%v", err)
	}
	if vm.ID != "vm123" {
		t.Fatalf("vm.ID=%q", vm.ID)
	}
}
