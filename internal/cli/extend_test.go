package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestLeaseTTLDefaultsAndMax(t *testing.T) {
	cfg := defaultConfig()
	if cfg.TTL != 30*time.Minute || cfg.IdleTimeout != 30*time.Minute || cfg.MaxTTL != 4*time.Hour {
		t.Fatalf("defaults TTL=%s idle=%s max=%s", cfg.TTL, cfg.IdleTimeout, cfg.MaxTTL)
	}
	for _, value := range []string{"0", "-1h", "25h", "invalid"} {
		if _, err := parseMaxTTL(value); err == nil {
			t.Errorf("accepted invalid max %q", value)
		}
	}
	clearConfigEnv(t)
	t.Setenv("CRABBOX_TTL", "24h")
	t.Setenv("CRABBOX_MAX_TTL", "2h")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TTL != 24*time.Hour || cfg.MaxTTL != 2*time.Hour {
		t.Fatalf("env values TTL=%s max=%s", cfg.TTL, cfg.MaxTTL)
	}
	cfg = clampDirectLeaseDurations(cfg)
	if cfg.TTL != 2*time.Hour {
		t.Fatalf("direct TTL=%s", cfg.TTL)
	}
}

func TestDirectLeasePolicyNotices(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ttl, idle   time.Duration
		coordinator bool
		want        string
	}{
		{name: "unchanged", ttl: time.Hour, idle: 30 * time.Minute},
		{name: "idle clamp", ttl: 30 * time.Minute, idle: time.Hour, want: "Lease policy clamped lifetime to 30m0s and idle timeout to 30m0s.\n"},
		{name: "maximum clamp", ttl: 24 * time.Hour, idle: 5 * time.Hour, want: "Lease policy clamped lifetime to 4h0m0s and idle timeout to 4h0m0s.\n"},
		{name: "broker reports its own policy", ttl: 24 * time.Hour, idle: 5 * time.Hour, coordinator: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Provider, cfg.TTL, cfg.IdleTimeout = "aws", tc.ttl, tc.idle
			cfg.Coordinator = ""
			if tc.coordinator {
				cfg.Coordinator = "https://broker.example.test"
			}
			var stderr bytes.Buffer
			(App{Stderr: &stderr}).reportDirectLeasePolicyNotice(cfg)
			if stderr.String() != tc.want {
				t.Fatalf("notice=%q want=%q", stderr.String(), tc.want)
			}
		})
	}
	var stderr bytes.Buffer
	app := App{Stderr: &stderr}
	requested := time.Hour
	server := Server{Labels: map[string]string{"idle_timeout_secs": "1800"}}
	app.reportIdleTimeoutPolicyNotice(&requested, server)
	app.reportIdleTimeoutPolicyNotice(nil, server)
	requested = 30 * time.Minute
	app.reportIdleTimeoutPolicyNotice(&requested, server)
	if stderr.String() != "Lease policy clamped idle timeout to 30m0s.\n" {
		t.Fatalf("heartbeat notice=%q", stderr.String())
	}
}

func TestExtendCommandFlagsAndBrokerBody(t *testing.T) {
	for _, test := range []struct {
		args  []string
		key   string
		value int
	}{
		{[]string{"cbx_123"}, "extendBySeconds", 1800},
		{[]string{"cbx_123", "--by", "45m"}, "extendBySeconds", 2700},
		{[]string{"cbx_123", "--ttl", "2h", "--reason", "finish tests", "--json"}, "ttlSeconds", 7200},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_PROVIDER", "aws")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/leases/cbx_123/extend" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body[test.key] != float64(test.value) || body["expectedProvider"] != "aws" {
					t.Errorf("body=%v", body)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_123", Provider: "aws", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), LeasePolicy: &CoordinatorLeasePolicy{MaxTTLSeconds: 14400, RuntimeExtension: true}}})
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "fixture-token")
			var out, stderr bytes.Buffer
			if err := (App{Stdout: &out, Stderr: &stderr}).Run(context.Background(), append([]string{"extend"}, test.args...)); err != nil {
				t.Fatalf("%v stderr=%s", err, &stderr)
			}
			if !strings.Contains(out.String(), "4h") {
				t.Fatalf("output=%s", &out)
			}
		})
	}
	for _, args := range [][]string{{}, {"id", "--by", "1m", "--ttl", "1h"}, {"id", "--by", "0s"}, {"id", "--ttl", "-1s"}, {"id", "--reason", strings.Repeat("x", 201)}} {
		if err := (App{Stdout: io.Discard, Stderr: io.Discard}).Run(context.Background(), append([]string{"extend"}, args...)); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestExtendDirectUnsupported(t *testing.T) {
	clearConfigEnv(t)
	heartbeatDirectBackendForTest = &heartbeatDirectBackend{}
	t.Cleanup(func() { heartbeatDirectBackendForTest = nil })
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).Run(context.Background(), []string{"extend", "cbx_123", "--provider", heartbeatDirectProviderName})
	if err == nil || !strings.Contains(err.Error(), "extend is not supported for provider") {
		t.Fatalf("error=%v", err)
	}
}

func TestExtendedLeaseLabels(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cfg := defaultConfig()
	labels := DirectLeaseLabels(cfg, "cbx_123", "fixture", "hetzner", "", true, now.Add(-20*time.Minute))
	labels["last_touched_at"] = LeaseLabelTime(now)
	next, err := ExtendedLeaseLabels(labels, cfg, LeaseExtendRequest{By: 30 * time.Minute}, now)
	if err != nil {
		t.Fatal(err)
	}
	if next["ttl_secs"] != "3600" || next["expires_at"] != LeaseLabelTime(now.Add(30*time.Minute)) {
		t.Fatalf("labels=%v", next)
	}
	if labels["ttl_secs"] != "1800" {
		t.Fatal("mutated input labels")
	}
	next, err = ExtendedLeaseLabels(next, cfg, LeaseExtendRequest{TTL: 24 * time.Hour}, now)
	if err != nil || next["ttl_secs"] != "14400" {
		t.Fatalf("labels=%v err=%v", next, err)
	}
	if _, err = ExtendedLeaseLabels(next, cfg, LeaseExtendRequest{By: time.Minute}, now); err == nil {
		t.Fatal("accepted non-advancing extension")
	}
}

func TestRunHeartbeatExtendOnlyDuringCommand(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(time.Duration(0).String()+strings.ToUpper(strings.TrimSpace(map[bool]string{false: "setup", true: "command"}[running])), func(t *testing.T) {
			var executing atomic.Bool
			executing.Store(running)
			received := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/control" {
					http.NotFound(w, r)
					return
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				select {
				case received <- body:
				default:
				}
				_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"aws"}}`))
			}))
			defer server.Close()
			client := CoordinatorClient{BaseURL: server.URL, Token: "fixture-token", Client: server.Client()}
			stop, err := startCoordinatorHeartbeat(t.Context(), &client, "cbx_123", "aws", 30*time.Minute, nil, nil, io.Discard, executing.Load)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			select {
			case body := <-received:
				if (body["extendWhileRunning"] == true) != running {
					t.Fatalf("body=%v running=%t", body, running)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("heartbeat timeout")
			}
		})
	}
	if _, ok := heartbeatRequestBody("aws", nil, nil)["extendWhileRunning"]; ok {
		t.Fatal("plain heartbeat requests extension")
	}
}

func TestLeasePolicyNoticePrintedOnce(t *testing.T) {
	clearConfigEnv(t)
	const notice = "Lease policy clamped lifetime to 14400s."
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_abcdef123456", Provider: "aws", State: "active", PolicyNotice: notice}})
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.Provider = "aws"
	cfg.Coordinator = server.URL
	cfg.CoordToken = "fixture-token"
	var stderr bytes.Buffer
	backend := coordinatorLeaseBackend{cfg: cfg, spec: ProviderSpec{Name: "aws"}, coord: &CoordinatorClient{BaseURL: server.URL, Token: "fixture-token", Client: server.Client()}, rt: Runtime{Stderr: &stderr}}
	_, err := backend.createCoordinatorLeaseWithProgress(t.Context(), cfg, "ssh-ed25519 fixture", false, "cbx_abcdef123456", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(stderr.String(), notice) != 1 {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRunHeartbeatBudgetNotice(t *testing.T) {
	const notice = "Lease will expire at 2026-10-13T12:30:00Z; budget exhausted."
	for _, transport := range []string{"control", "http"} {
		t.Run(transport, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/control" {
					if transport == "http" {
						http.NotFound(w, r)
						return
					}
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					if _, _, err := conn.Read(r.Context()); err != nil {
						t.Error(err)
						return
					}
					data, _ := json.Marshal(coordinatorControlMessage{Type: "heartbeat", OK: true, PolicyNotice: notice})
					_ = conn.Write(r.Context(), websocket.MessageText, data)
					_, _, _ = conn.Read(r.Context())
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_123", Provider: "aws", PolicyNotice: notice}})
			}))
			defer server.Close()
			messages := make(chan string, 2)
			client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
			stop, err := startCoordinatorHeartbeat(t.Context(), &client, "cbx_123", "aws", time.Minute, nil, nil, heartbeatWarningWriter{messages}, func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			select {
			case got := <-messages:
				if got != notice+"\n" {
					t.Fatalf("notice=%q", got)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("missing budget exhaustion notice")
			}
		})
	}
	var stderr bytes.Buffer
	printNotice := leasePolicyNoticePrinter(&stderr)
	printNotice("")
	printNotice(notice)
	printNotice(notice)
	printNotice(notice + " Updated headroom.")
	if stderr.String() != notice+"\n" {
		t.Fatalf("repeated notice: %q", stderr.String())
	}
}

func TestLeasePolicyNoticeKeepsBudgetWarningAfterClamp(t *testing.T) {
	const clamp = "Lease policy clamped idle timeout from 7200s to 1800s."
	const budget = "Lease will expire at 2026-10-13T12:30:00Z; budget exhausted."
	for _, notices := range [][]string{{clamp, budget}, {budget, clamp}} {
		var stderr bytes.Buffer
		printNotice := leasePolicyNoticePrinter(&stderr)
		for _, notice := range notices {
			printNotice(notice)
			printNotice(notice)
		}
		printNotice(budget + " Updated headroom.")
		if want := strings.Join(notices, "\n") + "\n"; stderr.String() != want {
			t.Fatalf("notices=%q, want %q", stderr.String(), want)
		}
	}
}
