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
	"reflect"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
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
	load := 0.42
	if _, err := client.FinishRun(context.Background(), "run_123", 1, time.Second, 2*time.Second, log, false, nil, &RunTelemetrySummary{End: &LeaseTelemetry{Load1: &load}}); err != nil {
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
	if got := finishBody["telemetry"].(map[string]any)["end"].(map[string]any)["load1"]; got != 0.42 {
		t.Fatalf("telemetry=%#v", finishBody["telemetry"])
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
	if body := heartbeatRequestBody(nil, nil); len(body) != 0 {
		t.Fatalf("touch heartbeat body=%v, want empty", body)
	}
	idleTimeout := 45 * time.Minute
	body := heartbeatRequestBody(&idleTimeout, nil)
	if body["idleTimeoutSeconds"] != 2700 {
		t.Fatalf("heartbeat body=%v, want idle timeout seconds", body)
	}
	load := 0.42
	body = heartbeatRequestBody(nil, &LeaseTelemetry{Load1: &load})
	if body["telemetry"] == nil {
		t.Fatalf("heartbeat body=%v, want telemetry", body)
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
	load := 0.42
	if _, err := client.TouchLeaseWithTelemetry(context.Background(), "cbx_123", &LeaseTelemetry{Load1: &load}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UpdateLeaseIdleTimeout(context.Background(), "cbx_123", 45*time.Minute); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 || bodies[0] != "{}" || !strings.Contains(bodies[1], `"load1":0.42`) || !strings.Contains(bodies[2], `"idleTimeoutSeconds":2700`) {
		t.Fatalf("heartbeat bodies=%q", bodies)
	}
}

func TestCoordinatorAppendRunTelemetry(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs/run_123/telemetry" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"run":{"id":"run_123","leaseID":"cbx_123","owner":"peter@example.com","org":"openclaw","provider":"aws","class":"standard","serverType":"t3.small","command":["sleep","60"],"state":"running","logBytes":0,"logTruncated":false,"startedAt":"2026-05-02T00:00:00Z"}}`))
	}))
	defer server.Close()
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	load := 0.42
	if _, err := client.AppendRunTelemetry(context.Background(), "run_123", &LeaseTelemetry{Load1: &load}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"telemetry"`) || !strings.Contains(body, `"load1":0.42`) {
		t.Fatalf("append telemetry body=%q", body)
	}
}

func TestCoordinatorHeartbeatTouchesImmediately(t *testing.T) {
	touches := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/control" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/v1/leases/cbx_123/heartbeat" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		touches <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"aws","state":"active","expiresAt":"2026-05-01T00:30:00Z"}}`))
	}))
	defer server.Close()

	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	stop := startCoordinatorHeartbeat(context.Background(), &client, "cbx_123", 30*time.Minute, nil, nil, io.Discard)
	defer stop()

	select {
	case <-touches:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat did not touch immediately")
	}
}

func TestCoordinatorHeartbeatIncludesTelemetry(t *testing.T) {
	bodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/control" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/v1/leases/cbx_123/heartbeat" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		bodies <- string(data)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"aws","state":"active","expiresAt":"2026-05-01T00:30:00Z"}}`))
	}))
	defer server.Close()

	load := 0.77
	collector := func(context.Context) (*LeaseTelemetry, error) {
		return &LeaseTelemetry{Load1: &load}, nil
	}
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	stop := startCoordinatorHeartbeat(context.Background(), &client, "cbx_123", 30*time.Minute, nil, collector, io.Discard)
	defer stop()

	select {
	case body := <-bodies:
		if !strings.Contains(body, `"load1":0.77`) {
			t.Fatalf("heartbeat body=%s, want telemetry", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat did not touch immediately")
	}
}

func TestCoordinatorHeartbeatUsesControlWebSocket(t *testing.T) {
	bodies := make(chan string, 1)
	httpHeartbeats := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/control":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept control websocket: %v", err)
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			_, data, err := conn.Read(r.Context())
			if err != nil {
				t.Errorf("read control heartbeat: %v", err)
				return
			}
			bodies <- string(data)
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"heartbeat","leaseID":"cbx_123","ok":true,"expiresAt":"2026-05-01T00:30:00Z"}`))
			<-r.Context().Done()
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_123/heartbeat":
			httpHeartbeats <- struct{}{}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"aws","state":"active","expiresAt":"2026-05-01T00:30:00Z"}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	load := 0.77
	collector := func(context.Context) (*LeaseTelemetry, error) {
		return &LeaseTelemetry{Load1: &load}, nil
	}
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	stop := startCoordinatorHeartbeat(context.Background(), &client, "cbx_123", 30*time.Minute, nil, collector, io.Discard)
	defer stop()

	select {
	case body := <-bodies:
		if !strings.Contains(body, `"type":"heartbeat"`) || !strings.Contains(body, `"load1":0.77`) {
			t.Fatalf("control heartbeat body=%s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat did not use control websocket")
	}
	select {
	case <-httpHeartbeats:
		t.Fatal("heartbeat fell back to HTTP despite websocket success")
	default:
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
		Provider           string   `json:"provider"`
		AWSSSHCIDRs        []string `json:"awsSSHCIDRs"`
		AzureLocation      string   `json:"azureLocation"`
		AzureImage         string   `json:"azureImage"`
		GCPProject         string   `json:"gcpProject"`
		GCPZone            string   `json:"gcpZone"`
		GCPNetwork         string   `json:"gcpNetwork"`
		GCPTags            []string `json:"gcpTags"`
		GCPSSHCIDRs        []string `json:"gcpSSHCIDRs"`
		GCPRootGB          int64    `json:"gcpRootGB"`
		SSHFallbackPorts   []string `json:"sshFallbackPorts"`
		ServerTypeExplicit bool     `json:"serverTypeExplicit"`
		Capacity           map[string]any
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
		Provider:           "google",
		ServerType:         "t3.small",
		ServerTypeExplicit: true,
		AWSSSHCIDRs:        []string{"198.51.100.7/32"},
		AzureLocation:      "eastus",
		AzureImage:         "Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest",
		GCPProject:         "crabbox-project",
		gcpProjectExplicit: true,
		GCPZone:            "europe-west2-b",
		GCPImage:           "projects/custom/global/images/crabbox",
		GCPNetwork:         "crabbox-net",
		GCPTags:            []string{"crabbox-ci"},
		GCPSSHCIDRs:        []string{"198.51.100.11/32"},
		GCPRootGB:          900,
		SSHFallbackPorts:   []string{"22", "2022"},
		Capacity: CapacityConfig{
			Market:   "spot",
			Strategy: "most-available",
			Fallback: "on-demand-after-120s",
			Hints:    true,
		},
	}, "ssh-ed25519 test", false, "cbx_123", "blue-crab")
	if err != nil {
		t.Fatal(err)
	}
	if len(body.AWSSSHCIDRs) != 1 || body.AWSSSHCIDRs[0] != "198.51.100.7/32" {
		t.Fatalf("awsSSHCIDRs=%v", body.AWSSSHCIDRs)
	}
	if body.Provider != "gcp" {
		t.Fatalf("provider=%q want canonical gcp", body.Provider)
	}
	if body.AzureLocation != "eastus" {
		t.Fatalf("azureLocation=%q", body.AzureLocation)
	}
	if body.AzureImage != "Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:latest" {
		t.Fatalf("azureImage=%q", body.AzureImage)
	}
	if body.GCPProject != "crabbox-project" || body.GCPZone != "europe-west2-b" || body.GCPNetwork != "crabbox-net" || body.GCPRootGB != 900 {
		t.Fatalf("unexpected gcp body: %#v", body)
	}
	if len(body.GCPTags) != 1 || body.GCPTags[0] != "crabbox-ci" || len(body.GCPSSHCIDRs) != 1 || body.GCPSSHCIDRs[0] != "198.51.100.11/32" {
		t.Fatalf("unexpected gcp tags/cidrs: tags=%v cidrs=%v", body.GCPTags, body.GCPSSHCIDRs)
	}
	if len(body.SSHFallbackPorts) != 2 || body.SSHFallbackPorts[0] != "22" || body.SSHFallbackPorts[1] != "2022" {
		t.Fatalf("sshFallbackPorts=%v", body.SSHFallbackPorts)
	}
	if !body.ServerTypeExplicit {
		t.Fatal("serverTypeExplicit=false, want true")
	}
	if body.Capacity != nil {
		t.Fatalf("default capacity fields should be omitted for mixed-version brokers: %#v", body.Capacity)
	}
}

func TestCoordinatorCreateLeaseOmitsAmbientGCPProject(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "developer-adc-project")

	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"gcp","state":"active","host":"192.0.2.10"}}`))
	}))
	defer server.Close()

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Provider = "gcp"
	cfg.ServerType = serverTypeForConfig(cfg)
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	if _, err := client.CreateLease(context.Background(), cfg, "ssh-ed25519 test", false, "cbx_123", "blue-crab"); err != nil {
		t.Fatal(err)
	}
	if cfg.GCPProject != "developer-adc-project" {
		t.Fatalf("test setup project=%q", cfg.GCPProject)
	}
	if _, ok := body["gcpProject"]; ok {
		t.Fatalf("ambient ADC project should be omitted so coordinator defaults apply: %#v", body)
	}
}

func TestCoordinatorCreateLeaseSendsConfiguredGCPProjectDespiteAmbientADC(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "crabbox.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "developer-adc-project")
	if err := os.WriteFile(configPath, []byte(`provider: gcp
gcp:
  project: configured-crabbox-project
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"gcp","state":"active","host":"192.0.2.10"}}`))
	}))
	defer server.Close()

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerType = serverTypeForConfig(cfg)
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	if _, err := client.CreateLease(context.Background(), cfg, "ssh-ed25519 test", false, "cbx_123", "blue-crab"); err != nil {
		t.Fatal(err)
	}
	if got := body["gcpProject"]; got != "configured-crabbox-project" {
		t.Fatalf("gcpProject=%#v body=%#v", got, body)
	}
}

func TestCoordinatorCreateLeaseSendsExplicitBuiltInGCPDefaults(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "gcp")
	t.Setenv("CRABBOX_GCP_ZONE", "europe-west2-a")
	t.Setenv("CRABBOX_GCP_IMAGE", defaultGCPLinuxImage)
	t.Setenv("CRABBOX_GCP_NETWORK", "default")
	t.Setenv("CRABBOX_GCP_TAGS", "crabbox-ssh")
	t.Setenv("CRABBOX_GCP_ROOT_GB", "400")

	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"gcp","state":"active","host":"192.0.2.10"}}`))
	}))
	defer server.Close()

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ServerType = serverTypeForConfig(cfg)
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	if _, err := client.CreateLease(context.Background(), cfg, "ssh-ed25519 test", false, "cbx_123", "blue-crab"); err != nil {
		t.Fatal(err)
	}
	if body["gcpZone"] != "europe-west2-a" || body["gcpImage"] != defaultGCPLinuxImage || body["gcpNetwork"] != "default" {
		t.Fatalf("explicit built-in string defaults not forwarded: %#v", body)
	}
	tags, ok := body["gcpTags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "crabbox-ssh" {
		t.Fatalf("explicit built-in tags not forwarded: %#v", body["gcpTags"])
	}
	if body["gcpRootGB"] != float64(400) {
		t.Fatalf("explicit built-in rootGB not forwarded: %#v", body["gcpRootGB"])
	}
}

func TestCoordinatorCreateLeaseOmitsBuiltInGCPDefaults(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"gcp","state":"active","host":"192.0.2.10"}}`))
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Provider = "gcp"
	cfg.ServerType = serverTypeForConfig(cfg)
	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	if _, err := client.CreateLease(context.Background(), cfg, "ssh-ed25519 test", false, "cbx_123", "blue-crab"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"gcpProject", "gcpZone", "gcpImage", "gcpNetwork", "gcpSubnet", "gcpTags", "gcpSSHCIDRs", "gcpRootGB", "gcpServiceAccount"} {
		if _, ok := body[key]; ok {
			t.Fatalf("%s should be omitted so coordinator defaults apply: %#v", key, body)
		}
	}
}

func TestCoordinatorCreateLeaseSendsConfiguredCapacityExtensions(t *testing.T) {
	var body struct {
		Capacity map[string]any
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
		Provider: "aws",
		Capacity: CapacityConfig{
			Market:            "spot",
			Strategy:          "most-available",
			Fallback:          "on-demand-after-120s",
			Regions:           []string{"eu-west-1", "eu-west-2"},
			AvailabilityZones: []string{"eu-west-1a"},
			Hints:             false,
		},
	}, "ssh-ed25519 test", false, "cbx_123", "blue-crab")
	if err != nil {
		t.Fatal(err)
	}
	if got := stringSliceFromJSON(body.Capacity["regions"]); !reflect.DeepEqual(got, []string{"eu-west-1", "eu-west-2"}) {
		t.Fatalf("capacity.regions=%v", got)
	}
	if got := stringSliceFromJSON(body.Capacity["availabilityZones"]); !reflect.DeepEqual(got, []string{"eu-west-1a"}) {
		t.Fatalf("capacity.availabilityZones=%v", got)
	}
	if got, ok := body.Capacity["hints"].(bool); !ok || got {
		t.Fatalf("capacity.hints=%#v, want false", body.Capacity["hints"])
	}
}

func TestCoordinatorLeaseDecodesLegacyCapacityResponse(t *testing.T) {
	var lease CoordinatorLease
	if err := json.Unmarshal([]byte(`{"id":"cbx_123","provider":"aws","serverType":"c7a.8xlarge"}`), &lease); err != nil {
		t.Fatal(err)
	}
	if lease.Market != "" || len(lease.ProvisioningAttempts) != 0 || len(lease.CapacityHints) != 0 {
		t.Fatalf("new capacity fields should be optional: %#v", lease)
	}
}

func TestCoordinatorLeaseDecodesProvisioningAttempts(t *testing.T) {
	var lease CoordinatorLease
	if err := json.Unmarshal([]byte(`{
		"id":"cbx_123",
		"provider":"aws",
		"serverType":"c7i.24xlarge",
		"requestedServerType":"c7a.48xlarge",
		"market":"on-demand",
		"provisioningAttempts":[{"region":"eu-west-1","serverType":"c7a.48xlarge","market":"spot","category":"policy","message":"not eligible"}],
		"capacityHints":[{"code":"aws_capacity_routed","message":"AWS launch routed to eu-west-2","action":"keep regions","region":"eu-west-2","market":"on-demand","class":"beast","serverType":"c7i.24xlarge","regionsTried":["eu-west-1","eu-west-2"]}]
	}`), &lease); err != nil {
		t.Fatal(err)
	}
	if lease.RequestedServerType != "c7a.48xlarge" || lease.ServerType != "c7i.24xlarge" {
		t.Fatalf("lease=%#v", lease)
	}
	if len(lease.ProvisioningAttempts) != 1 || lease.ProvisioningAttempts[0].Category != "policy" {
		t.Fatalf("attempts=%#v", lease.ProvisioningAttempts)
	}
	if lease.Market != "on-demand" || len(lease.CapacityHints) != 1 || lease.CapacityHints[0].Region != "eu-west-2" {
		t.Fatalf("capacity fields market=%q hints=%#v", lease.Market, lease.CapacityHints)
	}
}

func stringSliceFromJSON(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestCoordinatorFallbackSummary(t *testing.T) {
	summary := coordinatorFallbackSummary(CoordinatorLease{
		RequestedServerType: "c7a.48xlarge",
		ServerType:          "c7i.24xlarge",
		ProvisioningAttempts: []ProvisioningAttempt{{
			Region:     "eu-west-1",
			ServerType: "c7a.48xlarge",
			Market:     "spot",
			Category:   "policy",
			Message:    "not eligible",
		}},
	})
	if !strings.Contains(summary, "requested_type=c7a.48xlarge") || !strings.Contains(summary, "attempts=eu-west-1/c7a.48xlarge:policy") {
		t.Fatalf("summary=%q", summary)
	}
}

func TestCoordinatorCapacityHintLines(t *testing.T) {
	lines := coordinatorCapacityHintLines(CoordinatorLease{
		CapacityHints: []CapacityHint{{
			Code:    "aws_capacity_routed",
			Message: "AWS launch routed to eu-west-2",
			Action:  "keep multiple regions configured",
		}},
	})
	if len(lines) != 1 || !strings.Contains(lines[0], "aws_capacity_routed") || !strings.Contains(lines[0], "action=keep multiple regions") {
		t.Fatalf("lines=%#v", lines)
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
