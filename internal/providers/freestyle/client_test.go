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

func TestFreestyleListVMsPaginates(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got, want := r.URL.Query().Get("limit"), "100"; got != want {
			t.Errorf("limit=%q want %q", got, want)
		}
		switch got := r.URL.Query().Get("offset"); got {
		case "0":
			vms := make([]freestyleVM, freestyleListPageSize)
			for i := range vms {
				vms[i].ID = "page-one"
			}
			_ = json.NewEncoder(w).Encode(freestyleListVMsResponse{VMs: vms, TotalCount: freestyleListPageSize + 1})
		case "100":
			_ = json.NewEncoder(w).Encode(freestyleListVMsResponse{
				VMs:        []freestyleVM{{ID: "page-two"}},
				TotalCount: freestyleListPageSize + 1,
			})
		default:
			t.Errorf("unexpected offset %q", got)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := &freestyleHTTPClient{
		apiKey:     "test-key",
		apiURL:     server.URL,
		httpClient: server.Client(),
	}
	vms, err := client.ListVMs(context.Background())
	if err != nil {
		t.Fatalf("ListVMs err=%v", err)
	}
	if requests != 2 || len(vms) != freestyleListPageSize+1 || vms[len(vms)-1].ID != "page-two" {
		t.Fatalf("requests=%d vms=%d last=%q", requests, len(vms), vms[len(vms)-1].ID)
	}
}

func TestFreestyleDeleteVMTreatsNotFoundAsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Method, http.MethodDelete; got != want {
			t.Errorf("method=%s want %s", got, want)
		}
		if got, want := r.URL.EscapedPath(), "/v1/vms/vm123"; got != want {
			t.Errorf("path=%q want %q", got, want)
		}
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer server.Close()

	client := &freestyleHTTPClient{
		apiKey:     "test-key",
		apiURL:     server.URL,
		httpClient: server.Client(),
	}
	if err := client.DeleteVM(context.Background(), "vm123"); err != nil {
		t.Fatalf("DeleteVM err=%v, want nil for missing VM", err)
	}
}
