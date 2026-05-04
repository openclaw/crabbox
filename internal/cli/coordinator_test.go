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

func TestCoordinatorRunEvents(t *testing.T) {
	var createBody map[string]any
	var eventBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"run":{"id":"run_123","leaseID":"","owner":"peter@example.com","org":"openclaw","provider":"aws","class":"standard","serverType":"t3.small","command":["pnpm","test"],"state":"running","phase":"starting","logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/run_123/events":
			if err := json.NewDecoder(r.Body).Decode(&eventBody); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"event":{"runID":"run_123","seq":2,"type":"sync.started","phase":"sync","createdAt":"2026-05-02T00:00:01Z"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/run_123/events":
			if got := r.URL.Query().Get("after"); got != "4" {
				t.Fatalf("after query=%q", got)
			}
			if got := r.URL.Query().Get("limit"); got != "25" {
				t.Fatalf("limit query=%q", got)
			}
			_, _ = w.Write([]byte(`{"events":[{"runID":"run_123","seq":1,"type":"run.started","phase":"starting","createdAt":"2026-05-02T00:00:00Z"}]}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	run, err := client.CreateRun(context.Background(), "", Config{
		Provider:   "aws",
		Class:      "standard",
		ServerType: "t3.small",
	}, []string{"pnpm", "test"})
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != "run_123" || run.Phase != "starting" {
		t.Fatalf("run=%#v", run)
	}
	if got, ok := createBody["leaseID"].(string); !ok || got != "" {
		t.Fatalf("leaseID body=%#v", createBody["leaseID"])
	}
	event, err := client.AppendRunEvent(context.Background(), run.ID, CoordinatorRunEventInput{Type: "sync.started", Phase: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != "sync.started" || event.Seq != 2 {
		t.Fatalf("event=%#v", event)
	}
	if got, ok := eventBody["type"].(string); !ok || got != "sync.started" {
		t.Fatalf("event body=%#v", eventBody)
	}
	events, err := client.RunEvents(context.Background(), run.ID, 4, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "run.started" {
		t.Fatalf("events=%#v", events)
	}
}

func TestCoordinatorFinishRunSendsLogChunks(t *testing.T) {
	var finishBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs/run_123/finish" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&finishBody); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"run":{"id":"run_123","leaseID":"","owner":"peter@example.com","org":"openclaw","provider":"aws","class":"standard","serverType":"t3.small","command":["pnpm","test"],"state":"failed","phase":"failed","exitCode":1,"logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z"}}`))
	}))
	defer server.Close()
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	log := strings.Repeat("x", coordinatorRunLogChunkBytes) + "tail"
	if _, err := client.FinishRun(context.Background(), "run_123", 1, time.Second, 2*time.Second, log, false, nil); err != nil {
		t.Fatal(err)
	}
	chunks, ok := finishBody["logChunks"].([]any)
	if !ok {
		t.Fatalf("logChunks body=%#v", finishBody["logChunks"])
	}
	if len(chunks) != 2 {
		t.Fatalf("logChunks=%d, want 2", len(chunks))
	}
	if got := chunks[0].(string); len(got) != coordinatorRunLogChunkBytes {
		t.Fatalf("first chunk length=%d, want %d", len(got), coordinatorRunLogChunkBytes)
	}
	if got := chunks[1].(string); got != "tail" {
		t.Fatalf("second chunk=%q, want tail", got)
	}
	if got := finishBody["log"].(string); len(got) != runLogFallbackPreviewBytes || !strings.HasSuffix(got, "tail") {
		t.Fatalf("fallback log length=%d suffix=%q", len(got), got[len(got)-4:])
	}
}

func TestCurlConfigKeepsBearerTokenInConfig(t *testing.T) {
	client := CoordinatorClient{
		BaseURL: "https://example.test",
		Token:   "secret-token",
		Access: AccessConfig{
			ClientID:     "access-client",
			ClientSecret: "access-secret",
			Token:        "access-jwt",
		},
	}
	config, cleanup, err := client.curlConfig("POST", "/v1/leases", []byte(`{"leaseID":"cbx"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	for _, want := range []string{
		`url = "https://example.test/v1/leases"`,
		`request = "POST"`,
		`header = "Authorization: Bearer secret-token"`,
		`header = "CF-Access-Client-Id: access-client"`,
		`header = "CF-Access-Client-Secret: access-secret"`,
		`header = "cf-access-token: access-jwt"`,
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

func TestCoordinatorHTTPAddsAccessHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer broker-token" {
			t.Fatalf("Authorization=%q", got)
		}
		if got := r.Header.Get("CF-Access-Client-Id"); got != "access-client" {
			t.Fatalf("CF-Access-Client-Id=%q", got)
		}
		if got := r.Header.Get("CF-Access-Client-Secret"); got != "access-secret" {
			t.Fatalf("CF-Access-Client-Secret=%q", got)
		}
		if got := r.Header.Get("cf-access-token"); got != "access-jwt" {
			t.Fatalf("cf-access-token=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := CoordinatorClient{
		BaseURL: server.URL,
		Token:   "broker-token",
		Access: AccessConfig{
			ClientID:     "access-client",
			ClientSecret: "access-secret",
			Token:        "access-jwt",
		},
		Client: server.Client(),
	}
	if err := client.Health(context.Background()); err != nil {
		t.Fatal(err)
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

func TestCoordinatorHeartbeatTouchesImmediately(t *testing.T) {
	touches := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/leases/cbx_123/heartbeat" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		touches <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"aws","state":"active","expiresAt":"2026-05-01T00:30:00Z"}}`))
	}))
	defer server.Close()

	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	stop := startCoordinatorHeartbeat(context.Background(), &client, "cbx_123", 30*time.Minute, nil, io.Discard)
	defer stop()

	select {
	case <-touches:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat did not touch immediately")
	}
}

func TestCoordinatorLeaseWatchCancelsWhenLeaseReleased(t *testing.T) {
	oldInterval := coordinatorLeaseWatchInterval
	coordinatorLeaseWatchInterval = 10 * time.Millisecond
	defer func() { coordinatorLeaseWatchInterval = oldInterval }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/leases/cbx_123" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"aws","state":"released","expiresAt":"2026-05-01T00:30:00Z"}}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	stop := startCoordinatorLeaseWatch(ctx, &client, "cbx_123", cancel, io.Discard)
	defer stop()

	select {
	case <-ctx.Done():
		if cause := context.Cause(ctx); cause == nil || !strings.Contains(cause.Error(), "became released") {
			t.Fatalf("cause=%v, want released lease cause", cause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lease watcher did not cancel after release")
	}
}

func TestCoordinatorCreateLeaseSendsAWSSSHCIDRs(t *testing.T) {
	var body struct {
		AWSSSHCIDRs        []string `json:"awsSSHCIDRs"`
		SSHFallbackPorts   []string `json:"sshFallbackPorts"`
		ServerTypeExplicit bool     `json:"serverTypeExplicit"`
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
		Provider:           "aws",
		ServerType:         "t3.small",
		ServerTypeExplicit: true,
		AWSSSHCIDRs:        []string{"198.51.100.7/32"},
		SSHFallbackPorts:   []string{"22", "2022"},
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
	if !body.ServerTypeExplicit {
		t.Fatal("serverTypeExplicit=false, want true")
	}
}

func TestCoordinatorLeaseDecodesProvisioningAttempts(t *testing.T) {
	var lease CoordinatorLease
	if err := json.Unmarshal([]byte(`{
		"id":"cbx_123",
		"provider":"aws",
		"serverType":"c7i.24xlarge",
		"requestedServerType":"c7a.48xlarge",
		"provisioningAttempts":[{"serverType":"c7a.48xlarge","market":"spot","category":"policy","message":"not eligible"}]
	}`), &lease); err != nil {
		t.Fatal(err)
	}
	if lease.RequestedServerType != "c7a.48xlarge" || lease.ServerType != "c7i.24xlarge" {
		t.Fatalf("lease=%#v", lease)
	}
	if len(lease.ProvisioningAttempts) != 1 || lease.ProvisioningAttempts[0].Category != "policy" {
		t.Fatalf("attempts=%#v", lease.ProvisioningAttempts)
	}
}

func TestCoordinatorFallbackSummary(t *testing.T) {
	summary := coordinatorFallbackSummary(CoordinatorLease{
		RequestedServerType: "c7a.48xlarge",
		ServerType:          "c7i.24xlarge",
		ProvisioningAttempts: []ProvisioningAttempt{{
			ServerType: "c7a.48xlarge",
			Market:     "spot",
			Category:   "policy",
			Message:    "not eligible",
		}},
	})
	if !strings.Contains(summary, "requested_type=c7a.48xlarge") || !strings.Contains(summary, "attempts=c7a.48xlarge:policy") {
		t.Fatalf("summary=%q", summary)
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
	if promoted, err := client.PromoteImage(context.Background(), "ami-12345678"); err != nil || promoted.PromotedAt == "" {
		t.Fatalf("promoted=%#v err=%v", promoted, err)
	}
}

func TestLeaseStatusRequiresSSHReadiness(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/cbx_123" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","slug":"blue-crab","provider":"aws","target":"windows","windowsMode":"normal","state":"active","serverType":"m7i.4xlarge","host":"127.0.0.1","sshUser":"crabbox","sshPort":"22"}}`))
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
	if state.TargetOS != targetWindows || state.WindowsMode != windowsModeNormal {
		t.Fatalf("target=%s windowsMode=%s", state.TargetOS, state.WindowsMode)
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
