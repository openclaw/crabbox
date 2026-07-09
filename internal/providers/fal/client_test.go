package fal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientUsesOfficialComputePathsAndKeyAuth(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Key test-key" {
			t.Fatalf("Authorization=%q, want Key auth", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/compute/instances":
			if r.URL.Query().Get("limit") != "5" || r.URL.Query().Get("cursor") != "Mg==" {
				t.Fatalf("query=%s", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"next_cursor":null,"has_more":false,"instances":[{"id":"inst_abc123xyz","instance_type":"gpu_1x_h100_sxm5","region":"us-west","sector":"sector_1","ip":"203.0.113.42","status":"ready","creator_user_nickname":"developer"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/compute/instances/inst_abc123xyz":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"inst_abc123xyz","instance_type":"gpu_1x_h100_sxm5","region":"us-west","sector":"sector_1","ip":"203.0.113.42","status":"ready","creator_user_nickname":"developer"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/compute/instances":
			if got := r.Header.Get("Idempotency-Key"); got != "idem-1" {
				t.Fatalf("Idempotency-Key=%q", got)
			}
			var req CreateInstanceRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.InstanceType != InstanceTypeH100x8 || req.SSHKey != "ssh-ed25519 AAA test" || req.Sector != Sector2 {
				t.Fatalf("create request=%#v", req)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"inst_def456uvw","instance_type":"gpu_8x_h100_sxm5","region":"us-east","sector":"sector_2","status":"provisioning","creator_user_nickname":"developer"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/compute/instances/inst_def456uvw":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	api, err := newClient(Config{Fal: FalConfig{APIKey: "test-key", APIURL: server.URL}}, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	list, err := api.ListInstances(context.Background(), 5, "Mg==")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Instances) != 1 || list.Instances[0].ID != "inst_abc123xyz" || !list.Instances[0].Status.Known() {
		t.Fatalf("list=%#v", list)
	}
	instance, err := api.GetInstance(context.Background(), "inst_abc123xyz")
	if err != nil {
		t.Fatal(err)
	}
	if instance.IP != "203.0.113.42" || instance.Status != InstanceStatusReady {
		t.Fatalf("instance=%#v", instance)
	}
	created, err := api.CreateInstance(context.Background(), CreateInstanceRequest{InstanceType: InstanceTypeH100x8, SSHKey: "ssh-ed25519 AAA test", Sector: Sector2}, "idem-1")
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "inst_def456uvw" || created.Status != InstanceStatusProvisioning {
		t.Fatalf("created=%#v", created)
	}
	if err := api.DeleteInstance(context.Background(), "inst_def456uvw"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /compute/instances?cursor=Mg%3D%3D&limit=5",
		"GET /compute/instances/inst_abc123xyz",
		"POST /compute/instances",
		"DELETE /compute/instances/inst_def456uvw",
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests=\n%s\nwant=\n%s", strings.Join(seen, "\n"), strings.Join(want, "\n"))
	}
}

func TestClientDecodesStandardErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"type":"authorization_error","message":"Access denied","docs_url":"https://fal.ai/docs","request_id":"req_123"}}`))
	}))
	defer server.Close()
	api, err := newClient(Config{Fal: FalConfig{APIKey: "test-key", APIURL: server.URL}}, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.ListInstances(context.Background(), 0, "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err=%T %[1]v, want APIError", err)
	}
	if apiErr.StatusCode != http.StatusForbidden || apiErr.Type != "authorization_error" || apiErr.RequestID != "req_123" {
		t.Fatalf("apiErr=%#v", apiErr)
	}
}

func TestClientRedactsAPIKeyReflectedByErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"rejected Key test-key"}}`))
	}))
	defer server.Close()
	api, err := newClient(Config{Fal: FalConfig{APIKey: "test-key", APIURL: server.URL}}, Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.ListInstances(context.Background(), 0, "")
	if err == nil || strings.Contains(err.Error(), "test-key") || !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("error=%v", err)
	}
}

func TestAPIErrorFormatting(t *testing.T) {
	var nilErr *APIError
	if got := nilErr.Error(); got != "" {
		t.Fatalf("nil error=%q", got)
	}
	for name, tc := range map[string]struct {
		err  *APIError
		want string
	}{
		"typed":   {err: &APIError{Type: "quota", Message: "limit reached"}, want: "fal API quota: limit reached"},
		"status":  {err: &APIError{Status: "Forbidden"}, want: "fal API Forbidden"},
		"message": {err: &APIError{Message: "bad request"}, want: "fal API bad request"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error()=%q want %q", got, tc.want)
			}
		})
	}
}

func TestClientRejectsPlainHTTPExceptLoopback(t *testing.T) {
	if _, err := newClient(Config{Fal: FalConfig{APIKey: "test-key", APIURL: "http://api.fal.ai/v1"}}, Runtime{}); err == nil {
		t.Fatal("accepted non-loopback http")
	}
	_, err := newClient(Config{Fal: FalConfig{APIKey: "test-key", APIURL: "http://user:secret@api.fal.ai/v1"}}, Runtime{})
	if err == nil {
		t.Fatal("accepted non-loopback http with userinfo")
	}
	message := err.Error()
	if strings.Contains(message, "user") || strings.Contains(message, "secret") {
		t.Fatalf("api url error leaked userinfo: %q", message)
	}
	if _, err := newClient(Config{Fal: FalConfig{APIKey: "test-key", APIURL: "http://127.0.0.1:8080/v1"}}, Runtime{}); err != nil {
		t.Fatalf("loopback http rejected: %v", err)
	}
}

func TestClientRejectsCredentialBearingBaseURLComponents(t *testing.T) {
	for name, apiURL := range map[string]string{
		"userinfo": "https://user:secret@api.fal.ai/v1",
		"query":    "https://api.fal.ai/v1?token=secret",
		"fragment": "https://api.fal.ai/v1#secret",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newClient(Config{Fal: FalConfig{APIKey: "test-key", APIURL: apiURL}}, Runtime{})
			if err == nil {
				t.Fatal("accepted credential-bearing API URL")
			}
			if message := err.Error(); strings.Contains(message, "secret") || strings.Contains(message, "user") || strings.Contains(message, "token") {
				t.Fatalf("api url error leaked sensitive component: %q", message)
			}
		})
	}
}

func TestClientRefusesCrossOriginRedirectBeforeReplayingAuth(t *testing.T) {
	var redirectedAuth string
	untrusted := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		redirectedAuth = r.Header.Get("Authorization")
	}))
	defer untrusted.Close()
	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, untrusted.URL+"/compute/instances?token=redirect-secret", http.StatusTemporaryRedirect)
	}))
	defer trusted.Close()
	api, err := newClient(Config{Fal: FalConfig{APIKey: "test-key", APIURL: trusted.URL}}, Runtime{HTTP: trusted.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.ListInstances(context.Background(), 0, "")
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("redirect err=%v", err)
	}
	if strings.Contains(err.Error(), "redirect-secret") || strings.Contains(err.Error(), untrusted.URL) {
		t.Fatalf("redirect error leaked target: %v", err)
	}
	if redirectedAuth != "" {
		t.Fatalf("auth replayed to untrusted origin: %q", redirectedAuth)
	}
}

func TestUnknownStatusIsNotKnown(t *testing.T) {
	if InstanceStatus("booting").Known() {
		t.Fatal("unexpected status should not be known")
	}
}
