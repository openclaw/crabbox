package unikraftcloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClientConfig(apiURL string) Config {
	cfg := Config{}
	cfg.UnikraftCloud.APIKey = "ukc-test-key"
	cfg.UnikraftCloud.APIURL = apiURL
	return cfg
}

func TestUnikraftCloudClientRequiresAPIKey(t *testing.T) {
	cfg := Config{}
	cfg.UnikraftCloud.Metro = "fra"
	if _, err := newUnikraftCloudClient(cfg, Runtime{}); err == nil {
		t.Fatal("newUnikraftCloudClient accepted empty API key")
	}
}

func TestUnikraftCloudBaseURLFromMetro(t *testing.T) {
	for _, test := range []struct {
		name    string
		metro   string
		apiURL  string
		want    string
		wantErr string
	}{
		{name: "fra", metro: "fra", want: "https://api.fra.unikraft.cloud"},
		{name: "dal", metro: "dal", want: "https://api.dal.unikraft.cloud"},
		{name: "uppercase normalized", metro: "SIN", want: "https://api.sin.unikraft.cloud"},
		{name: "missing metro", metro: "", wantErr: "requires a metro"},
		{name: "invalid metro", metro: "fra.evil.example/", wantErr: "is invalid"},
		{name: "explicit URL wins", metro: "fra", apiURL: "https://ukc.example.com", want: "https://ukc.example.com"},
		{name: "explicit URL trailing slash", metro: "fra", apiURL: "https://ukc.example.com/", want: "https://ukc.example.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{}
			cfg.UnikraftCloud.Metro = test.metro
			cfg.UnikraftCloud.APIURL = test.apiURL
			got, err := unikraftCloudBaseURL(cfg)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unikraftCloudBaseURL: %v", err)
			}
			if got != test.want {
				t.Fatalf("baseURL = %q, want %q", got, test.want)
			}
		})
	}
}

func TestUnikraftCloudClientRejectsUnsafeAPIURLs(t *testing.T) {
	for _, test := range []struct {
		name   string
		apiURL string
		secret string
	}{
		{name: "plain http", apiURL: "http://api.fra.unikraft.cloud"},
		{name: "userinfo", apiURL: "https://user:url-secret@api.fra.unikraft.cloud", secret: "url-secret"},
		{name: "query", apiURL: "https://api.fra.unikraft.cloud?token=query-secret", secret: "query-secret"},
		{name: "fragment", apiURL: "https://api.fra.unikraft.cloud#fragment-secret", secret: "fragment-secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := newUnikraftCloudClient(testClientConfig(test.apiURL), Runtime{})
			if err == nil {
				t.Fatalf("newUnikraftCloudClient accepted unsafe URL %q", test.apiURL)
			}
			if test.secret != "" && strings.Contains(err.Error(), test.secret) {
				t.Fatalf("error leaked URL secret: %v", err)
			}
		})
	}
}

func TestUnikraftCloudClientLifecycleEndpoints(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ukc-test-key" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "invalid token"})
			return
		}
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/instances":
			var req createInstanceRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Image != "unikraft.org/nginx:latest" || !req.Autostart || req.MemoryMB != 256 {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "bad create body"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data": map[string]any{"instances": []map[string]any{{
					"uuid":  "11111111-2222-3333-4444-555555555555",
					"name":  "funky-town",
					"state": "starting",
				}}},
			})
		case "GET /v1/instances/11111111-2222-3333-4444-555555555555":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data": map[string]any{"instances": []map[string]any{{
					"uuid":         "11111111-2222-3333-4444-555555555555",
					"name":         "funky-town",
					"state":        "running",
					"private_fqdn": "funky-town.internal",
					"service_group": map[string]any{
						"domains": []map[string]any{{"fqdn": "funky-town.fra.unikraft.app"}},
					},
				}}},
			})
		case "GET /v1/instances":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data": map[string]any{"instances": []map[string]any{
					{"uuid": "11111111-2222-3333-4444-555555555555", "name": "funky-town", "state": "running"},
					{"uuid": "66666666-7777-8888-9999-000000000000", "name": "quiet-village", "state": "stopped"},
				}},
			})
		case "PUT /v1/instances/11111111-2222-3333-4444-555555555555/stop":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data": map[string]any{"instances": []map[string]any{{
					"uuid":  "11111111-2222-3333-4444-555555555555",
					"state": "stopped",
				}}},
			})
		case "DELETE /v1/instances/11111111-2222-3333-4444-555555555555":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success"})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "no such endpoint"})
		}
	}))
	defer server.Close()

	api, err := newUnikraftCloudClient(testClientConfig(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatalf("newUnikraftCloudClient: %v", err)
	}
	ctx := context.Background()

	created, err := api.CreateInstance(ctx, createInstanceRequest{
		Image:     "unikraft.org/nginx:latest",
		MemoryMB:  256,
		Autostart: true,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if created.UUID != "11111111-2222-3333-4444-555555555555" || created.State != "starting" {
		t.Fatalf("created = %#v", created)
	}

	got, err := api.GetInstance(ctx, created.UUID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.State != "running" || instanceFQDN(got) != "funky-town.fra.unikraft.app" {
		t.Fatalf("instance = %#v", got)
	}

	instances, err := api.ListInstances(ctx)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 2 || instances[1].Name != "quiet-village" {
		t.Fatalf("instances = %#v", instances)
	}

	stopped, err := api.StopInstance(ctx, created.UUID)
	if err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	if stopped.State != "stopped" {
		t.Fatalf("stopped = %#v", stopped)
	}

	if err := api.DeleteInstance(ctx, created.UUID); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}

	want := []string{
		"POST /v1/instances",
		"GET /v1/instances/11111111-2222-3333-4444-555555555555",
		"GET /v1/instances",
		"PUT /v1/instances/11111111-2222-3333-4444-555555555555/stop",
		"DELETE /v1/instances/11111111-2222-3333-4444-555555555555",
	}
	if len(requests) != len(want) {
		t.Fatalf("requests = %#v", requests)
	}
	for i, path := range want {
		if requests[i] != path {
			t.Fatalf("requests[%d] = %q, want %q", i, requests[i], path)
		}
	}
}

func TestUnikraftCloudClientErrorClassification(t *testing.T) {
	for _, test := range []struct {
		name            string
		handler         http.HandlerFunc
		wantNotFound    bool
		wantUnauth      bool
		wantErrContains string
	}{
		{
			name: "http 404",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "instance not found"})
			},
			wantNotFound:    true,
			wantErrContains: "instance not found",
		},
		{
			name: "http 401",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "invalid token"})
			},
			wantUnauth:      true,
			wantErrContains: "invalid token",
		},
		{
			name: "error envelope on 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "error",
					"errors": []map[string]any{{"status": 404, "message": "no instance with that uuid"}},
				})
			},
			wantNotFound:    true,
			wantErrContains: "no instance with that uuid",
		},
		{
			name: "per-item error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "success",
					"data": map[string]any{"instances": []map[string]any{{
						"status":  "error",
						"error":   404,
						"message": "instance already deleted",
					}}},
				})
			},
			wantNotFound:    true,
			wantErrContains: "instance already deleted",
		},
		{
			name: "non-json 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte("<html>proxy error</html>"))
			},
			wantErrContains: "expected application/json",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			api, err := newUnikraftCloudClient(testClientConfig(server.URL), Runtime{HTTP: server.Client()})
			if err != nil {
				t.Fatalf("newUnikraftCloudClient: %v", err)
			}
			_, err = api.GetInstance(context.Background(), "11111111-2222-3333-4444-555555555555")
			if err == nil {
				t.Fatal("GetInstance succeeded, want error")
			}
			if isNotFound(err) != test.wantNotFound {
				t.Fatalf("isNotFound(%v) = %v, want %v", err, isNotFound(err), test.wantNotFound)
			}
			if isUnauthorized(err) != test.wantUnauth {
				t.Fatalf("isUnauthorized(%v) = %v, want %v", err, isUnauthorized(err), test.wantUnauth)
			}
			if !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("err = %v, want containing %q", err, test.wantErrContains)
			}
		})
	}
}

func TestUnikraftCloudClientDoesNotLeakAPIKeyInErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "bad key ukc-test-key rejected"})
	}))
	defer server.Close()
	api, err := newUnikraftCloudClient(testClientConfig(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatalf("newUnikraftCloudClient: %v", err)
	}
	_, err = api.ListInstances(context.Background())
	if err == nil {
		t.Fatal("ListInstances succeeded, want error")
	}
	if strings.Contains(err.Error(), "ukc-test-key") {
		t.Fatalf("error leaked API key: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error did not redact API key: %v", err)
	}
}

func TestUnikraftCloudClientRefusesCrossOriginRedirect(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success"})
	}))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/instances", http.StatusFound)
	}))
	defer server.Close()
	api, err := newUnikraftCloudClient(testClientConfig(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatalf("newUnikraftCloudClient: %v", err)
	}
	_, err = api.ListInstances(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("err = %v, want cross-origin redirect refusal", err)
	}
	if strings.Contains(err.Error(), "ukc-test-key") {
		t.Fatalf("error leaked API key: %v", err)
	}
}
