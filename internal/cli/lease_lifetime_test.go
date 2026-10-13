package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCoordinatorLifetimePolicySurvivesDecoding(t *testing.T) {
	const payload = `{"id":"cbx_abcdef123456","lifetimePolicy":{"mode":"enforce","source":"admin-canary","deadlineAt":"2026-10-11T10:00:00Z","effectiveStopAt":"2026-10-11T10:05:00Z","retainSeconds":300,"status":"retained","deleteAfterAt":"2026-10-11T10:10:00Z","warnings":["DELETION: retained root disk will be deleted"],"readiness":{"status":"ready","checkedAt":"2026-10-11T09:59:00Z"}}}`
	var lease CoordinatorLease
	if err := json.Unmarshal([]byte(payload), &lease); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &view); err != nil {
		t.Fatal(err)
	}
	var policy map[string]any
	if err := json.Unmarshal(view["lifetimePolicy"], &policy); err != nil {
		t.Fatalf("coordinator decoding discarded lifetimePolicy: %s (%v)", encoded, err)
	}
	for key, want := range map[string]string{"source": "admin-canary", "status": "retained", "effectiveStopAt": "2026-10-11T10:05:00Z", "deleteAfterAt": "2026-10-11T10:10:00Z"} {
		if policy[key] != want {
			t.Errorf("%s=%v, want %s", key, policy[key], want)
		}
	}
	if warnings, ok := policy["warnings"].([]any); !ok || len(warnings) != 1 {
		t.Fatalf("warnings were lost: %v", policy)
	}
}

func TestLifetimeCanaryFlags(t *testing.T) {
	for _, tc := range []struct {
		name      string
		command   string
		args      []string
		configure func(*Config)
		target    leaseFlagTarget
		wantError string
	}{
		{name: "warmup", command: "warmup"},
		{name: "run", command: "run"},
		{name: "no maximum", args: []string{"--lifetime-retention", "5m"}, wantError: "supplied together"},
		{name: "no retention", args: []string{"--lifetime-canary", "6m"}, wantError: "supplied together"},
		{name: "too short", args: []string{"--lifetime-canary", "59s", "--lifetime-retention", "5m"}, wantError: "whole number of seconds"},
		{name: "too long", args: []string{"--lifetime-canary", "745h", "--lifetime-retention", "5m"}, wantError: "whole number of seconds"},
		{name: "fractional", args: []string{"--lifetime-canary", "60.1s", "--lifetime-retention", "5m"}, wantError: "whole number of seconds"},
		{name: "negative retention", args: []string{"--lifetime-canary", "6m", "--lifetime-retention", "-1m"}, wantError: "whole number of seconds"},
		{name: "minimum", args: []string{"--lifetime-canary", "60s", "--lifetime-retention", "60s"}},
		{name: "maximum", args: []string{"--lifetime-canary", "744h", "--lifetime-retention", "744h"}},
		{name: "reuse", target: leaseFlagTarget{ID: "cbx_abcdef123456", Reuse: true}, wantError: "existing lease reuse"},
		{name: "direct", configure: func(cfg *Config) { cfg.Coordinator = "" }, wantError: "coordinator-managed AWS"},
		{name: "registered", configure: func(cfg *Config) { cfg.BrokerMode = BrokerModeRegistered }, wantError: "coordinator-managed AWS"},
		{name: "other provider", configure: func(cfg *Config) { cfg.Provider = "gcp" }, wantError: "coordinator-managed AWS"},
		{name: "spot", args: []string{"--lifetime-canary", "6m", "--lifetime-retention", "5m", "--market", "spot"}, wantError: "on-demand"},
		{name: "other command", command: "prewarm", wantError: "new warmup or run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			cfg.Provider, cfg.Coordinator, cfg.Capacity.Market = "aws", "https://coordinator.example.test", "on-demand"
			cfg.CoordAdminToken, cfg.CoordToken = "fixture-admin", "fixture-user"
			if tc.configure != nil {
				tc.configure(&cfg)
			}
			fs := newFlagSet(blank(tc.command, "warmup"), io.Discard)
			values := registerLeaseCreateFlags(fs, cfg)
			args := tc.args
			if args == nil {
				args = []string{"--lifetime-canary", "6m", "--lifetime-retention", "5m"}
			}
			if err := parseFlags(fs, args); err != nil {
				t.Fatal(err)
			}
			err := applyLeaseCreateFlagsForTarget(&cfg, fs, values, tc.target, false)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("err=%v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LifetimePolicy == nil || cfg.LifetimePolicy.Mode != "enforce" || cfg.LifetimePolicy.MaxAgeSeconds != int(values.LifetimeCanary.Seconds()) || cfg.LifetimePolicy.RetainSeconds != int(values.LifetimeRetention.Seconds()) {
				t.Fatalf("policy=%+v", cfg.LifetimePolicy)
			}
			if cfg.CoordToken != "fixture-admin" || len(cfg.CoordTokenCommand) != 0 {
				t.Fatal("explicit canary did not select configured admin authentication")
			}
		})
	}
}

func TestLifetimeCanaryAbsentPreservesOrdinaryCreate(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	cfg.CoordToken, cfg.CoordAdminToken = "fixture-user", "fixture-admin"
	fs := newFlagSet("warmup", io.Discard)
	values := registerLeaseCreateFlags(fs, cfg)
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.LifetimePolicy != nil || cfg.CoordToken != "fixture-user" {
		t.Fatalf("ordinary create changed: policy=%+v token changed=%t", cfg.LifetimePolicy, cfg.CoordToken != "fixture-user")
	}
}

func TestCoordinatorCreateSendsLifetimeCanaryOnlyWhenRequested(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		for _, enrolled := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{true: "fixed", false: "ordinary"}[fixed], map[bool]string{true: "canary", false: "control"}[enrolled]}, "/"), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if _, ok := body["lifetimePolicy"]; ok != enrolled {
						t.Errorf("policy present=%t, want %t", ok, enrolled)
					}
					if enrolled && string(body["lifetimePolicy"]) != `{"mode":"enforce","maxAgeSeconds":360,"retainSeconds":300}` {
						t.Errorf("policy=%s", body["lifetimePolicy"])
					}
					method, path := http.MethodPost, "/v1/leases"
					if fixed {
						method, path = http.MethodPut, "/v1/leases/cbx_abcdef123456"
					}
					if r.Method != method || r.URL.Path != path {
						t.Errorf("request=%s %s", r.Method, r.URL.Path)
					}
					io.WriteString(w, `{"lease":{"id":"cbx_abcdef123456","provider":"aws"}}`)
				}))
				defer server.Close()
				cfg := baseConfig()
				cfg.Provider = "aws"
				if enrolled {
					cfg.LifetimePolicy = &LeaseLifetimeRequest{Mode: "enforce", MaxAgeSeconds: 360, RetainSeconds: 300}
				}
				client := &CoordinatorClient{BaseURL: server.URL, Token: "fixture", Client: server.Client()}
				var err error
				if fixed {
					_, err = client.EnsureLease(t.Context(), cfg, "ssh-ed25519 fixture", true, "cbx_abcdef123456", "canary")
				} else {
					_, err = client.CreateLease(t.Context(), cfg, "ssh-ed25519 fixture", true, "cbx_abcdef123456", "canary")
				}
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestLifetimePolicyVisibleInLeaseCommands(t *testing.T) {
	policy := &LeaseLifetimePolicy{Mode: "enforce", Source: "admin-canary", DeadlineAt: "2026-10-11T10:00:00Z", EffectiveStopAt: "2026-10-11T10:05:00Z", RetainSeconds: 300, DeleteAfterAt: "2026-10-11T10:10:00Z", Status: "retained", Warnings: []string{"DELETION: root disk will be deleted"}, Readiness: &LeaseLifetimeReadiness{Status: "ready"}}
	for _, command := range []string{"status", "inspect", "list", "admin leases"} {
		for _, jsonOut := range []bool{false, true} {
			t.Run(command+map[bool]string{true: "/json", false: "/human"}[jsonOut], func(t *testing.T) {
				clearConfigEnv(t)
				lease := CoordinatorLease{ID: "cbx_abcdef123456", Provider: "aws", TargetOS: targetLinux, State: "released", Keep: true, LifetimePolicy: policy}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet {
						t.Errorf("mutating request: %s", r.Method)
					}
					if r.URL.Path == "/v1/leases" || r.URL.Path == "/v1/admin/leases" {
						json.NewEncoder(w).Encode(map[string]any{"leases": []CoordinatorLease{lease}})
					} else if r.URL.Path == "/v1/leases/cbx_abcdef123456" {
						json.NewEncoder(w).Encode(map[string]any{"lease": lease})
					} else {
						t.Errorf("unexpected path %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				t.Setenv("CRABBOX_COORDINATOR", server.URL)
				t.Setenv("CRABBOX_COORDINATOR_TOKEN", "fixture")
				var stdout, stderr bytes.Buffer
				args := strings.Fields(command)
				if command == "status" || command == "inspect" {
					args = append(args, "--id", lease.ID, "--provider", "aws")
				} else if command == "list" {
					args = append(args, "--provider", "aws", "--retained")
				}
				if jsonOut {
					args = append(args, "--json")
				}
				if err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), args); err != nil {
					t.Fatalf("%v; stderr=%s", err, stderr.String())
				}
				if jsonOut {
					var record map[string]json.RawMessage
					if command == "list" || command == "admin leases" {
						var records []map[string]json.RawMessage
						if err := json.Unmarshal(stdout.Bytes(), &records); err != nil || len(records) != 1 {
							t.Fatalf("%s; err=%v", stdout.String(), err)
						}
						record = records[0]
					} else if err := json.Unmarshal(stdout.Bytes(), &record); err != nil {
						t.Fatal(err)
					}
					var got LeaseLifetimePolicy
					if err := json.Unmarshal(record["lifetimePolicy"], &got); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(&got, policy) {
						t.Fatalf("policy=%+v, want %+v", got, policy)
					}
				} else {
					for _, want := range []string{"admin-canary", "retained", "2026-10-11T10:05:00Z", "2026-10-11T10:10:00Z", "DELETION:", "lifetime_readiness=ready"} {
						if !strings.Contains(stdout.String(), want) {
							t.Errorf("missing %q: %s", want, stdout.String())
						}
					}
				}
			})
		}
	}
}
