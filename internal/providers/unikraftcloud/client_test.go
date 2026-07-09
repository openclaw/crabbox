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
		{name: "non-root path", apiURL: "https://api.fra.unikraft.cloud/proxy"},
		{name: "ambiguous double slash path", apiURL: "https://api.fra.unikraft.cloud//"},
		{name: "encoded non-root path", apiURL: "https://api.fra.unikraft.cloud/%2e"},
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
		case "DELETE /v1/instances/11111111-2222-3333-4444-555555555555":
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req["timeout_s"] != float64(-1) || req["dont_retain"] != true {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "bad delete body"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data": map[string]any{"instances": []map[string]any{{
					"status": "success",
					"uuid":   "11111111-2222-3333-4444-555555555555",
					"name":   "funky-town",
					"state":  "destroyed",
				}}},
			})
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
	if got.State != "running" || got.ServiceGroup == nil || len(got.ServiceGroup.Domains) != 1 || got.ServiceGroup.Domains[0].FQDN != "funky-town.fra.unikraft.app" {
		t.Fatalf("instance = %#v", got)
	}

	instances, err := api.ListInstances(ctx)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 2 || instances[1].Name != "quiet-village" {
		t.Fatalf("instances = %#v", instances)
	}

	deleted, err := api.DeleteInstance(ctx, created.UUID)
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if deleted.UUID != created.UUID || deleted.State != "destroyed" {
		t.Fatalf("deleted = %#v", deleted)
	}

	want := []string{
		"POST /v1/instances",
		"GET /v1/instances/11111111-2222-3333-4444-555555555555",
		"GET /v1/instances",
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
			name: "live missing instance envelope on 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status":  "error",
					"message": "Failed to perform all operations",
					"data": map[string]any{"instances": []map[string]any{{
						"status":  "error",
						"uuid":    "11111111-2222-3333-4444-555555555555",
						"message": "No instance with uuid '11111111-2222-3333-4444-555555555555'",
						"error":   8,
					}}},
				})
			},
			wantNotFound:    true,
			wantErrContains: "No instance with uuid",
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

func TestUnikraftCloudDeleteRejectsMalformedSuccessEnvelope(t *testing.T) {
	for _, test := range []struct {
		name         string
		body         string
		wantRedacted bool
	}{
		{name: "missing status", body: `{}`},
		{name: "unknown status", body: `{"status":"pending"}`},
		{name: "secret in status", body: `{"status":"ukc-test-key"}`, wantRedacted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			api, err := newUnikraftCloudClient(testClientConfig(server.URL), Runtime{HTTP: server.Client()})
			if err != nil {
				t.Fatalf("newUnikraftCloudClient: %v", err)
			}
			_, err = api.DeleteInstance(context.Background(), "11111111-2222-3333-4444-555555555555")
			if err == nil || !strings.Contains(err.Error(), "invalid status") {
				t.Fatalf("DeleteInstance err = %v, want malformed response rejection", err)
			}
			if strings.Contains(err.Error(), "ukc-test-key") {
				t.Fatalf("DeleteInstance leaked API key: %v", err)
			}
			if test.wantRedacted && !strings.Contains(err.Error(), "[redacted]") {
				t.Fatalf("DeleteInstance err = %v, want redaction marker", err)
			}
		})
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

func TestUnikraftCloudUserUUID(t *testing.T) {
	for _, test := range []struct {
		name            string
		body            string
		want            string
		wantErrContains string
		wantNotFound    bool
	}{
		{
			name: "one quota",
			body: `{"status":"success","data":{"quotas":[{"uuid":"11111111-2222-3333-4444-555555555555","status":"success"}]}}`,
			want: "11111111-2222-3333-4444-555555555555",
		},
		{
			name: "duplicate matching quotas",
			body: `{"status":"success","data":{"quotas":[{"uuid":"11111111-2222-3333-4444-555555555555"},{"uuid":"11111111-2222-3333-4444-555555555555"}]}}`,
			want: "11111111-2222-3333-4444-555555555555",
		},
		{
			name:            "no quota identity",
			body:            `{"status":"success","data":{"quotas":[{"uuid":""}]}}`,
			wantErrContains: "no user UUID",
		},
		{
			name:            "malformed quota identity",
			body:            `{"status":"success","data":{"quotas":[{"uuid":" 11111111-2222-3333-4444-555555555555 "}]}}`,
			wantErrContains: "invalid user UUID",
		},
		{
			name:            "conflicting identities",
			body:            `{"status":"success","data":{"quotas":[{"uuid":"11111111-2222-3333-4444-555555555555"},{"uuid":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}]}}`,
			wantErrContains: "conflicting user UUIDs",
		},
		{
			name:            "partial top-level status",
			body:            `{"status":"partial_success","message":"quota lookup incomplete","data":{"quotas":[{"uuid":"11111111-2222-3333-4444-555555555555"}]}}`,
			wantErrContains: "quota lookup incomplete",
		},
		{
			name:            "quota item error",
			body:            `{"status":"success","data":{"quotas":[{"status":"error","error":8,"message":"user not found"}]}}`,
			wantErrContains: "user not found",
			wantNotFound:    true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/users/quotas" || r.Header.Get("Authorization") != "Bearer ukc-test-key" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"status":"error","message":"unexpected quotas request"}`))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			api, err := newUnikraftCloudClient(testClientConfig(server.URL), Runtime{HTTP: server.Client()})
			if err != nil {
				t.Fatalf("newUnikraftCloudClient: %v", err)
			}
			got, err := api.UserUUID(context.Background())
			if test.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrContains) {
					t.Fatalf("UserUUID err = %v, want containing %q", err, test.wantErrContains)
				}
				if isNotFound(err) != test.wantNotFound {
					t.Fatalf("isNotFound(%v) = %v, want %v", err, isNotFound(err), test.wantNotFound)
				}
				return
			}
			if err != nil {
				t.Fatalf("UserUUID: %v", err)
			}
			if got != test.want {
				t.Fatalf("UserUUID = %q, want %q", got, test.want)
			}
		})
	}
}

func TestUnikraftCloudClientRejectsPartialInstanceResults(t *testing.T) {
	for _, test := range []struct {
		name            string
		body            string
		wantErrContains string
		wantNotFound    bool
	}{
		{
			name:            "per-item failure",
			body:            `{"status":"partial_success","message":"batch incomplete","data":{"instances":[{"status":"success","uuid":"11111111-2222-3333-4444-555555555555"},{"status":"error","error":8,"message":"requested instance vanished"}]}}`,
			wantErrContains: "requested instance vanished",
			wantNotFound:    true,
		},
		{
			name:            "unexplained partial success",
			body:            `{"status":"partial_success","message":"batch incomplete","data":{"instances":[{"status":"success","uuid":"11111111-2222-3333-4444-555555555555"}]}}`,
			wantErrContains: "batch incomplete",
		},
		{
			name:            "errors array on success",
			body:            `{"status":"success","errors":[{"status":503,"message":"metro unavailable"}],"data":{"instances":[]}}`,
			wantErrContains: "metro unavailable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			api, err := newUnikraftCloudClient(testClientConfig(server.URL), Runtime{HTTP: server.Client()})
			if err != nil {
				t.Fatalf("newUnikraftCloudClient: %v", err)
			}
			_, err = api.ListInstances(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("ListInstances err = %v, want containing %q", err, test.wantErrContains)
			}
			if isNotFound(err) != test.wantNotFound {
				t.Fatalf("isNotFound(%v) = %v, want %v", err, isNotFound(err), test.wantNotFound)
			}
		})
	}
}

func TestUnikraftCloudSingleInstanceOperationsRequireExactResult(t *testing.T) {
	const requestedID = "11111111-2222-3333-4444-555555555555"
	for _, test := range []struct {
		name            string
		body            string
		call            func(unikraftCloudAPI) error
		wantErrContains string
	}{
		{
			name: "create duplicate results",
			body: `{"status":"success","data":{"instances":[{"uuid":"11111111-2222-3333-4444-555555555555","name":"wanted"},{"uuid":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","name":"wanted"}]}}`,
			call: func(api unikraftCloudAPI) error {
				_, err := api.CreateInstance(context.Background(), createInstanceRequest{Name: "wanted", Image: "nginx:latest"})
				return err
			},
			wantErrContains: "expected exactly one",
		},
		{
			name: "create name mismatch",
			body: `{"status":"success","data":{"instances":[{"uuid":"11111111-2222-3333-4444-555555555555","name":"other"}]}}`,
			call: func(api unikraftCloudAPI) error {
				_, err := api.CreateInstance(context.Background(), createInstanceRequest{Name: "wanted", Image: "nginx:latest"})
				return err
			},
			wantErrContains: "unexpected instance name",
		},
		{
			name: "create invalid uuid",
			body: `{"status":"success","data":{"instances":[{"uuid":"not-a-uuid","name":"wanted"}]}}`,
			call: func(api unikraftCloudAPI) error {
				_, err := api.CreateInstance(context.Background(), createInstanceRequest{Name: "wanted", Image: "nginx:latest"})
				return err
			},
			wantErrContains: "invalid instance uuid",
		},
		{
			name: "get duplicate results",
			body: `{"status":"success","data":{"instances":[{"uuid":"11111111-2222-3333-4444-555555555555"},{"uuid":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}]}}`,
			call: func(api unikraftCloudAPI) error {
				_, err := api.GetInstance(context.Background(), requestedID)
				return err
			},
			wantErrContains: "expected exactly one",
		},
		{
			name: "get identity mismatch",
			body: `{"status":"success","data":{"instances":[{"uuid":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","name":"11111111-2222-3333-4444-555555555555"}]}}`,
			call: func(api unikraftCloudAPI) error {
				_, err := api.GetInstance(context.Background(), requestedID)
				return err
			},
			wantErrContains: "unexpected instance identity",
		},
		{
			name: "get name identity does not match uuid field",
			body: `{"status":"success","data":{"instances":[{"uuid":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","name":"other"}]}}`,
			call: func(api unikraftCloudAPI) error {
				_, err := api.GetInstance(context.Background(), "wanted-name")
				return err
			},
			wantErrContains: "unexpected instance identity",
		},
		{
			name: "delete missing result",
			body: `{"status":"success","data":{"instances":[]}}`,
			call: func(api unikraftCloudAPI) error {
				_, err := api.DeleteInstance(context.Background(), requestedID)
				return err
			},
			wantErrContains: "expected exactly one",
		},
		{
			name: "delete identity mismatch",
			body: `{"status":"success","data":{"instances":[{"status":"success","uuid":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","name":"other"}]}}`,
			call: func(api unikraftCloudAPI) error {
				_, err := api.DeleteInstance(context.Background(), requestedID)
				return err
			},
			wantErrContains: "unexpected instance identity",
		},
		{
			name: "delete missing explicit item success",
			body: `{"status":"success","data":{"instances":[{"uuid":"11111111-2222-3333-4444-555555555555","name":"wanted"}]}}`,
			call: func(api unikraftCloudAPI) error {
				_, err := api.DeleteInstance(context.Background(), requestedID)
				return err
			},
			wantErrContains: "without explicit success",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			api, err := newUnikraftCloudClient(testClientConfig(server.URL), Runtime{HTTP: server.Client()})
			if err != nil {
				t.Fatalf("newUnikraftCloudClient: %v", err)
			}
			err = test.call(api)
			if err == nil || !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("operation err = %v, want containing %q", err, test.wantErrContains)
			}
		})
	}
}

func TestUnikraftCloudClientRefusesSameOriginRedirectOutsideAPIPath(t *testing.T) {
	var redirected bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			redirected = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":{"instances":[]}}`))
			return
		}
		http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	api, err := newUnikraftCloudClient(testClientConfig(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatalf("newUnikraftCloudClient: %v", err)
	}
	_, err = api.ListInstances(context.Background())
	if err == nil || !strings.Contains(err.Error(), "outside the trusted API path") {
		t.Fatalf("err = %v, want trusted API path refusal", err)
	}
	if redirected {
		t.Fatal("client sent a request outside the trusted API path")
	}
}

func TestUnikraftCloudClientRefusesEncodedRedirectTraversal(t *testing.T) {
	var redirected bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/instances" {
			redirected = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":{"instances":[]}}`))
			return
		}
		w.Header().Set("Location", "/v1/%2e%2e/login")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	api, err := newUnikraftCloudClient(testClientConfig(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatalf("newUnikraftCloudClient: %v", err)
	}
	_, err = api.ListInstances(context.Background())
	if err == nil || !strings.Contains(err.Error(), "outside the trusted API path") {
		t.Fatalf("err = %v, want encoded traversal refusal", err)
	}
	if redirected {
		t.Fatal("client followed an encoded traversal redirect")
	}
}

func TestUnikraftCloudClientRefusesRedirectMutationMethodChange(t *testing.T) {
	var redirected bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/instances/delete-result" {
			redirected = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":{"instances":[]}}`))
			return
		}
		http.Redirect(w, r, "/v1/instances/delete-result", http.StatusFound)
	}))
	defer server.Close()
	api, err := newUnikraftCloudClient(testClientConfig(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatalf("newUnikraftCloudClient: %v", err)
	}
	_, err = api.DeleteInstance(context.Background(), "11111111-2222-3333-4444-555555555555")
	if err == nil || !strings.Contains(err.Error(), "changed mutation method from DELETE to GET") {
		t.Fatalf("err = %v, want mutation method refusal", err)
	}
	if redirected {
		t.Fatal("client followed a redirect that changed the mutation method")
	}
}

func TestUnikraftCloudClientAllowsMethodPreservingMutationRedirect(t *testing.T) {
	const instanceID = "11111111-2222-3333-4444-555555555555"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/instances/"+instanceID {
			http.Redirect(w, r, "/v1/instances/delete-result", http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path != "/v1/instances/delete-result" || r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","message":"redirect changed request"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer ukc-test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":"error","message":"missing auth"}`))
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["timeout_s"] != float64(-1) || request["dont_retain"] != true {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","message":"redirect lost body"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"instances": []map[string]any{{"uuid": instanceID, "status": "success"}}},
		})
	}))
	defer server.Close()
	api, err := newUnikraftCloudClient(testClientConfig(server.URL), Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatalf("newUnikraftCloudClient: %v", err)
	}
	deleted, err := api.DeleteInstance(context.Background(), instanceID)
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if deleted.UUID != instanceID {
		t.Fatalf("deleted = %#v", deleted)
	}
}
