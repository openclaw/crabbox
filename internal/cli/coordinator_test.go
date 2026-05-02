package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCoordinatorMachineIDAcceptsStringOrNumber(t *testing.T) {
	for name, input := range map[string]string{
		"string": `{"id":"i-123","labels":{}}`,
		"number": `{"id":128694755,"labels":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var machine CoordinatorMachine
			if err := json.Unmarshal([]byte(input), &machine); err != nil {
				t.Fatal(err)
			}
			if machine.ID == "" {
				t.Fatalf("machine ID was empty")
			}
		})
	}
}

func TestSplitCurlResponseParsesTrailingStatus(t *testing.T) {
	body, status, err := splitCurlResponse([]byte("{\"ok\":true}\n200"))
	if err != nil {
		t.Fatal(err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
}

func TestDecodeCoordinatorResponseCanReadTextBody(t *testing.T) {
	var buf bytes.Buffer
	if err := decodeCoordinatorResponse("GET", "/v1/runs/run_1/logs", 200, strings.NewReader("hello"), &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hello" {
		t.Fatalf("body=%q", buf.String())
	}
}

func TestCurlConfigKeepsBearerTokenInConfig(t *testing.T) {
	client := CoordinatorClient{BaseURL: "https://example.test", Token: "secret-token"}
	config, cleanup, err := client.curlConfig("POST", "/v1/leases", []byte(`{"leaseID":"cbx"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	for _, want := range []string{
		`url = "https://example.test/v1/leases"`,
		`request = "POST"`,
		`header = "Authorization: Bearer secret-token"`,
		`header = "Content-Type: application/json"`,
		`data-binary = "@`,
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("config missing %q:\n%s", want, config)
		}
	}
	bodyPath := curlConfigValueForTest(t, config, "data-binary")
	bodyPath = strings.TrimPrefix(bodyPath, "@")
	if _, err := os.Stat(bodyPath); err != nil {
		t.Fatalf("body file missing: %v", err)
	}
}

func TestHeartbeatRequestBodyOmitsIdleTimeoutForTouch(t *testing.T) {
	if body := heartbeatRequestBody(nil); len(body) != 0 {
		t.Fatalf("touch heartbeat body=%v, want empty", body)
	}
	idleTimeout := 45 * time.Minute
	body := heartbeatRequestBody(&idleTimeout)
	if body["idleTimeoutSeconds"] != 2700 {
		t.Fatalf("heartbeat body=%v, want idle timeout seconds", body)
	}
}

func TestCoordinatorTouchAndUpdateHeartbeatBodies(t *testing.T) {
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/leases/cbx_123/heartbeat" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(data))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"aws","state":"active","expiresAt":"2026-05-01T00:30:00Z"}}`))
	}))
	defer server.Close()
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	if _, err := client.TouchLease(context.Background(), "cbx_123"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UpdateLeaseIdleTimeout(context.Background(), "cbx_123", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0] != "{}" || !strings.Contains(bodies[1], `"idleTimeoutSeconds":2700`) {
		t.Fatalf("heartbeat bodies=%q", bodies)
	}
}

func TestCoordinatorCreateLeaseSendsAWSSSHCIDRs(t *testing.T) {
	var body struct {
		AWSSSHCIDRs      []string `json:"awsSSHCIDRs"`
		SSHFallbackPorts []string `json:"sshFallbackPorts"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"aws","state":"active","host":"192.0.2.10"}}`))
	}))
	defer server.Close()

	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	_, err := client.CreateLease(context.Background(), Config{
		Provider:         "aws",
		AWSSSHCIDRs:      []string{"198.51.100.7/32"},
		SSHFallbackPorts: []string{"22", "2022"},
	}, "ssh-ed25519 test", false, "cbx_123", "blue-crab")
	if err != nil {
		t.Fatal(err)
	}
	if len(body.AWSSSHCIDRs) != 1 || body.AWSSSHCIDRs[0] != "198.51.100.7/32" {
		t.Fatalf("awsSSHCIDRs=%v", body.AWSSSHCIDRs)
	}
	if len(body.SSHFallbackPorts) != 2 || body.SSHFallbackPorts[0] != "22" || body.SSHFallbackPorts[1] != "2022" {
		t.Fatalf("sshFallbackPorts=%v", body.SSHFallbackPorts)
	}
}

func TestCoordinatorImageCreateAndPromote(t *testing.T) {
	var createBody struct {
		LeaseID  string `json:"leaseID"`
		Name     string `json:"name"`
		NoReboot bool   `json:"noReboot"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/images":
			if r.Method != http.MethodPost {
				t.Fatalf("method=%s", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"image":{"id":"ami-12345678","name":"openclaw-crabbox-test","state":"pending","region":"eu-west-1"}}`))
		case "/v1/images/ami-12345678":
			_, _ = w.Write([]byte(`{"image":{"id":"ami-12345678","name":"openclaw-crabbox-test","state":"available","region":"eu-west-1"}}`))
		case "/v1/images/ami-12345678/promote":
			if r.Method != http.MethodPost {
				t.Fatalf("method=%s", r.Method)
			}
			_, _ = w.Write([]byte(`{"image":{"id":"ami-12345678","name":"openclaw-crabbox-test","state":"available","region":"eu-west-1","promotedAt":"2026-05-01T12:46:00Z"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	created, err := client.CreateImage(context.Background(), "cbx_123", "openclaw-crabbox-test", true)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "ami-12345678" || createBody.LeaseID != "cbx_123" || createBody.Name != "openclaw-crabbox-test" || !createBody.NoReboot {
		t.Fatalf("created=%#v body=%#v", created, createBody)
	}
	if image, err := client.Image(context.Background(), "ami-12345678"); err != nil || image.State != "available" {
		t.Fatalf("image=%#v err=%v", image, err)
	}
	if promoted, err := client.PromoteImage(context.Background(), "ami-12345678", ""); err != nil || promoted.PromotedAt == "" {
		t.Fatalf("promoted=%#v err=%v", promoted, err)
	}
}

func TestLeaseStatusRequiresSSHReadiness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/cbx_123" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","slug":"blue-crab","provider":"aws","state":"active","serverType":"c7a.8xlarge","host":"127.0.0.1","sshUser":"ubuntu","sshPort":"22"}}`))
	}))
	defer server.Close()

	state, err := (App{}).leaseStatus(context.Background(), Config{
		Coordinator: server.URL,
		Provider:    "aws",
		SSHKey:      filepath.Join(t.TempDir(), "missing-key"),
	}, "cbx_123")
	if err != nil {
		t.Fatal(err)
	}
	if !state.HasHost {
		t.Fatalf("HasHost=false, want true")
	}
	if state.Ready {
		t.Fatalf("Ready=true, want false when ssh readiness probe fails")
	}
}

func curlConfigValueForTest(t *testing.T, config, key string) string {
	t.Helper()
	prefix := key + " = "
	for _, line := range strings.Split(config, "\n") {
		if strings.HasPrefix(line, prefix) {
			var value string
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &value); err != nil {
				t.Fatal(err)
			}
			return value
		}
	}
	t.Fatalf("config key %q missing:\n%s", key, config)
	return ""
}
