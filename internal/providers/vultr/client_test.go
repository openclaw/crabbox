package vultr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestVultrClientCreateInstanceRequestShape(t *testing.T) {
	var requests []struct {
		Method string
		Path   string
		Auth   string
		Body   map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		requests = append(requests, struct {
			Method string
			Path   string
			Auth   string
			Body   map[string]any
		}{Method: r.Method, Path: r.URL.RequestURI(), Auth: r.Header.Get("Authorization"), Body: body})

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/ssh-keys":
			_, _ = w.Write([]byte(`{"ssh_keys":[],"meta":{"links":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/ssh-keys":
			_, _ = w.Write([]byte(`{"ssh_key":{"id":"key-123","name":"crabbox-cbx-abcdef123456","ssh_key":"ssh-ed25519 test"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/instances":
			label, _ := body["label"].(string)
			if body["region"] != "sjc" || body["plan"] != "vc2-2c-2gb" || body["os_id"].(float64) != 2284 {
				t.Fatalf("unexpected create body: %v", body)
			}
			if body["firewall_group_id"] != "fw-123" {
				t.Fatalf("firewall_group_id=%v", body["firewall_group_id"])
			}
			vpcs, _ := body["attach_vpc"].([]any)
			if len(vpcs) != 2 || vpcs[0] != "vpc-a" || vpcs[1] != "vpc-b" {
				t.Fatalf("attach_vpc=%v", body["attach_vpc"])
			}
			keys, _ := body["sshkey_id"].([]any)
			if len(keys) != 1 || keys[0] != "key-123" {
				t.Fatalf("sshkey_id=%v", body["sshkey_id"])
			}
			if body["activation_email"] != false || body["user_scheme"] != "limited" {
				t.Fatalf("activation/user_scheme body=%v", body)
			}
			userData, ok := body["user_data"].(string)
			if !ok {
				t.Fatalf("user_data missing: %v", body)
			}
			decoded, err := base64.StdEncoding.DecodeString(userData)
			if err != nil || !strings.Contains(string(decoded), "ssh-ed25519 test") {
				t.Fatalf("user_data not base64 cloud-init: decoded=%q err=%v", decoded, err)
			}
			tags, _ := body["tags"].([]any)
			if !containsVultrTag(tags, tagCrabbox) || !containsVultrTag(tags, "crabbox:provider:vultr") {
				t.Fatalf("tags=%v", tags)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"instance": map[string]any{"id": "inst-123", "label": label, "status": "pending", "tags": []string{"crabbox"}}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("VULTR_API_KEY", "test-token-redact-me")
	client, err := newVultrClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "vc2-2c-2gb"
	cfg.Vultr.Region = "sjc"
	cfg.Vultr.OS = "2284"
	cfg.Vultr.FirewallGroup = "fw-123"
	cfg.Vultr.VPCIDs = []string{"vpc-a", "vpc-b"}
	cfg.Vultr.UserScheme = "limited"

	if _, err := client.CreateInstance(context.Background(), cfg, "ssh-ed25519 test", "cbx_abcdef123456", "blue", false, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	for _, req := range requests {
		if req.Auth != "Bearer test-token-redact-me" {
			t.Fatalf("%s %s auth=%q", req.Method, req.Path, req.Auth)
		}
	}
}

func TestVultrClientCursorPaginationAndRateLimit(t *testing.T) {
	var slept []time.Duration
	var firstPageCalls int
	var baseURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.RequestURI() {
		case "/instances":
			firstPageCalls++
			if firstPageCalls == 1 {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "slow down", http.StatusTooManyRequests)
				return
			}
			_, _ = w.Write([]byte(`{"instances":[{"id":"one","label":"foreign"}],"meta":{"links":{"next":"` + baseURL + `/instances?cursor=next"}}}`))
		case "/instances?cursor=next":
			_, _ = w.Write([]byte(`{"instances":[{"id":"two","label":"cbx_abc-blue","tags":["crabbox","crabbox:provider:vultr","crabbox:target:linux","crabbox:lease:cbx_abc","crabbox:slug:blue"]}],"meta":{"links":{}}}`))
		default:
			t.Fatalf("unexpected request %s", r.URL.RequestURI())
		}
	}))
	defer server.Close()
	baseURL = server.URL

	t.Setenv("VULTR_API_KEY", "test-token-redact-me")
	client, err := newVultrClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	client.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	instances, err := client.ListCrabboxInstances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != time.Second {
		t.Fatalf("slept=%v", slept)
	}
	if len(instances) != 1 || instances[0].ID != "two" {
		t.Fatalf("instances=%#v", instances)
	}
}

func TestVultrClientRedactsSecretsFromErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `token=test-token-redact-me default_password=example-password user_data=example-user-data`, http.StatusForbidden)
	}))
	defer server.Close()

	t.Setenv("VULTR_API_KEY", "test-token-redact-me")
	client, err := newVultrClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	err = client.do(context.Background(), http.MethodGet, "/account", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	text := err.Error()
	for _, leaked := range []string{"test-token-redact-me", "default_password", "user_data"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("error leaked %q: %s", leaked, text)
		}
	}
}

func TestVultrClientAccountIDFallsBackToHashedEmailWhenOrganizationsFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/account":
			_, _ = w.Write([]byte(`{"account":{"email":"alice@example.com"}}`))
		case "/organizations":
			http.Error(w, "not permitted", http.StatusForbidden)
		default:
			t.Fatalf("unexpected request %s", r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("VULTR_API_KEY", "test-token-redact-me")
	client, err := newVultrClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	accountID, err := client.AccountID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(accountID, "account-email-sha256:") || strings.Contains(accountID, "alice") || strings.Contains(accountID, "@") {
		t.Fatalf("accountID=%q", accountID)
	}
}

func TestVultrClientDecodesLargeSuccessfulResponses(t *testing.T) {
	var systems []map[string]any
	for i := 0; i < 250; i++ {
		systems = append(systems, map[string]any{"id": 1000 + i, "name": "Other OS padding padding padding padding"})
	}
	systems = append(systems, map[string]any{"id": 2284, "name": "Ubuntu 24.04 x64"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/os" {
			t.Fatalf("unexpected request %s", r.URL.RequestURI())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"os": systems, "meta": map[string]any{"links": map[string]any{}}})
	}))
	defer server.Close()

	t.Setenv("VULTR_API_KEY", "test-token-redact-me")
	client, err := newVultrClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	osID, err := client.resolveUbuntuOS(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if osID != 2284 {
		t.Fatalf("osID=%d", osID)
	}
}

func TestVultrClientRejectsMultipleBootSources(t *testing.T) {
	t.Setenv("VULTR_API_KEY", "test-token-redact-me")
	client, err := newVultrClient(core.Runtime{HTTP: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	cfg := core.BaseConfig()
	cfg.Vultr.OS = "2284"
	cfg.Vultr.Image = "image-123"
	_, err = client.createInstanceBody(context.Background(), cfg, "ssh-ed25519 test", "key", "cbx_abc", "blue", false, time.Now())
	if err == nil || !strings.Contains(err.Error(), "exactly one boot source") {
		t.Fatalf("err=%v", err)
	}
}

func containsVultrTag(tags []any, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}
