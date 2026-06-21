package freestyle

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFreestyleClientRedactsAPIKeyFromAllResponseErrors(t *testing.T) {
	const apiKey = "fs_test_response_secret"
	tests := []struct {
		name   string
		invoke func(*freestyleHTTPClient) error
	}{
		{name: "create vm", invoke: func(client *freestyleHTTPClient) error {
			_, err := client.CreateVM(context.Background(), freestyleCreateVMRequest{Name: "test"})
			return err
		}},
		{name: "get vm", invoke: func(client *freestyleHTTPClient) error {
			_, err := client.GetVM(context.Background(), "vm123")
			return err
		}},
		{name: "list vms", invoke: func(client *freestyleHTTPClient) error {
			_, err := client.ListVMs(context.Background())
			return err
		}},
		{name: "delete vm", invoke: func(client *freestyleHTTPClient) error {
			return client.DeleteVM(context.Background(), "vm123")
		}},
		{name: "exec", invoke: func(client *freestyleHTTPClient) error {
			_, err := client.Exec(context.Background(), "vm123", "true", io.Discard, io.Discard)
			return err
		}},
		{name: "write file", invoke: func(client *freestyleHTTPClient) error {
			return client.WriteFile(context.Background(), "vm123", "/tmp/file", "content", "utf8")
		}},
		{name: "read file", invoke: func(client *freestyleHTTPClient) error {
			_, err := client.ReadFile(context.Background(), "vm123", "/tmp/file")
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got, want := r.Header.Get("Authorization"), "Bearer "+apiKey; got != want {
					t.Errorf("authorization header = %q, want %q", got, want)
				}
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, "rejected Bearer "+apiKey+" (key "+apiKey+")")
			}))
			defer server.Close()
			client := &freestyleHTTPClient{apiKey: apiKey, apiURL: server.URL, httpClient: server.Client()}

			err := tt.invoke(client)
			if err == nil {
				t.Fatal("expected response error")
			}
			if strings.Contains(err.Error(), apiKey) {
				t.Fatalf("API key leaked in response error: %v", err)
			}
			if !strings.Contains(err.Error(), "Bearer [redacted] (key [redacted])") {
				t.Fatalf("response error was not redacted: %v", err)
			}
		})
	}
}

func TestValidateFreestyleAPIURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "https", raw: "HTTPS://API.FREESTYLE.SH:443/", want: "https://api.freestyle.sh"},
		{name: "loopback", raw: "http://127.0.0.1:8080/api/", want: "http://127.0.0.1:8080/api"},
		{name: "localhost", raw: "http://localhost:8080", want: "http://localhost:8080"},
		{name: "remote http", raw: "http://api.freestyle.sh", wantErr: "must use HTTPS"},
		{name: "relative", raw: "/api", wantErr: "absolute HTTPS URL"},
		{name: "userinfo", raw: "https://user:pass@api.freestyle.sh", wantErr: "must not contain userinfo"},
		{name: "query", raw: "https://api.freestyle.sh?token=secret", wantErr: "must not contain userinfo"},
		{name: "fragment", raw: "https://api.freestyle.sh/#secret", wantErr: "must not contain userinfo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateFreestyleAPIURL(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateFreestyleAPIURL(%q) err=%v, want %q", tt.raw, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("validateFreestyleAPIURL(%q)=%q want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestFreestyleClientRefusesCrossOriginRedirect(t *testing.T) {
	var attackerRequests int
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerRequests++
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("authorization leaked to redirect target: %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/stolen", http.StatusFound)
	}))
	defer trusted.Close()

	api, err := newFreestyleClient(Config{
		Freestyle: FreestyleConfig{
			APIKey: "test-key",
			APIURL: trusted.URL,
		},
	}, Runtime{HTTP: trusted.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.ListVMs(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("ListVMs err=%v, want cross-origin redirect refusal", err)
	}
	if attackerRequests != 0 {
		t.Fatalf("redirect target received %d requests", attackerRequests)
	}
}

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
		if _, ok := body["template"]; ok {
			t.Fatalf("template should be omitted from default create request: %#v", body)
		}
		ports, ok := body["ports"].([]any)
		if !ok || len(ports) != 0 {
			t.Fatalf("ports=%#v want explicit empty array", body["ports"])
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

func TestFreestyleCreateVMNestsCustomSizing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if _, ok := body["vcpuCount"]; ok {
			t.Fatalf("vcpuCount must not be top-level: %#v", body)
		}
		if _, ok := body["memSizeGb"]; ok {
			t.Fatalf("memSizeGb must not be top-level: %#v", body)
		}
		template, ok := body["template"].(map[string]any)
		if !ok || template["vcpuCount"] != float64(4) || template["memSizeGb"] != float64(8) {
			t.Fatalf("template=%#v want nested custom sizing", body["template"])
		}
		ports, ok := body["ports"].([]any)
		if !ok || len(ports) != 0 {
			t.Fatalf("ports=%#v want explicit empty array", body["ports"])
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
	_, err := client.CreateVM(context.Background(), freestyleCreateVMRequest{
		Name:  "crabbox-test",
		Ports: []freestylePortMapping{},
		Template: &freestyleCreateVMTemplate{
			VcpuCount: 4,
			MemSizeGb: 8,
		},
	})
	if err != nil {
		t.Fatalf("CreateVM err=%v", err)
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
