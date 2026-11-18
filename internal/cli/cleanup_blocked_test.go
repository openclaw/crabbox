package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoordinatorReleaseBlockedCleanup(t *testing.T) {
	const id = "cbx_abcdef123456"
	lease := CoordinatorLease{ID: id, Provider: "azure", State: "released", CleanupStatus: "blocked"}
	if !coordinatorReleaseCleanupFailed(lease) {
		t.Fatal("unknown blocked status must fail closed for existing clients")
	}
	_, err := observeCoordinatorReleaseCompletion(context.Background(), nil, lease, id, "azure", nil)
	if err == nil {
		t.Fatal("blocked cleanup must preserve local artifacts")
	}
	for _, want := range []string{"automatic retries stopped", "inspect", "delete or restore", "crabbox stop", "local claim and SSH artifacts were preserved"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "after coordinator cleanup advances") {
		t.Errorf("blocked cleanup implies automatic progress: %v", err)
	}
}

func TestAdminLeasesBlockedCleanup(t *testing.T) {
	clearConfigEnv(t)
	isolateTestUserDirs(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"leases": []map[string]any{
			{"id": "cbx_blocked", "state": "released", "cleanupStatus": "blocked", "cleanupBlockedAt": "2026-10-01T02:00:00Z", "cleanupError": "resource identity changed"},
			{"id": "cbx_deferred", "state": "active", "deferredCleanupBlocked": map[string]string{"name": "old-candidate", "error": "ownership changed", "blockedAt": "2026-10-01T03:00:00Z"}},
			{"id": "cbx_failed", "state": "released", "cleanupStatus": "failed", "cleanupError": "HTTP 503"},
			{"id": "cbx_healthy", "state": "active"},
		}})
	}))
	defer server.Close()
	config, err := json.Marshal(map[string]any{"broker": map[string]string{"url": server.URL, "token": "synthetic-session"}})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	var out bytes.Buffer
	if err := (App{Stdout: &out, Stderr: &out}).adminLeases(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cleanup=blocked since=2026-10-01T02:00:00Z error=resource identity changed", "cleanup=blocked since=2026-10-01T03:00:00Z error=ownership changed", "cleanup=failed error=HTTP 503"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q: %s", want, &out)
		}
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "cbx_healthy") && strings.Contains(line, "cleanup=") {
			t.Errorf("healthy lease has cleanup summary: %s", line)
		}
	}
}

func TestInspectStatusBlockedCleanup(t *testing.T) {
	for _, command := range []string{"inspect", "status"} {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(command+map[bool]string{false: "/text", true: "/json"}[jsonOutput], func(t *testing.T) {
				clearConfigEnv(t)
				isolateTestUserDirs(t)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
						ID: "cbx_abcdef123456", Provider: "azure", State: "released", CleanupStatus: "blocked", CloudID: "old-candidate",
						CleanupRefusedSince: "2026-10-01T00:00:00Z", CleanupBlockedAt: "2026-10-01T02:00:00Z", CleanupError: "resource identity changed",
						DeferredCleanupBlocked: &CoordinatorDeferredCleanupBlocked{Name: "old-candidate", Error: "resource identity changed", BlockedAt: "2026-10-01T02:00:00Z"},
					}})
				}))
				defer server.Close()
				t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
				t.Setenv("CRABBOX_COORDINATOR", server.URL)
				t.Setenv("CRABBOX_COORDINATOR_TOKEN", "synthetic-session")
				var out bytes.Buffer
				app := App{Stdout: &out, Stderr: &out}
				args := []string{"--provider", "azure", "--id", "cbx_abcdef123456"}
				if jsonOutput {
					args = append(args, "--json")
				}
				var err error
				if command == "inspect" {
					err = app.inspect(t.Context(), args)
				} else {
					err = app.status(t.Context(), args)
				}
				if err != nil {
					t.Fatal(err)
				}
				if jsonOutput {
					var record StatusView
					if err := json.Unmarshal(out.Bytes(), &record); err != nil {
						t.Fatal(err)
					}
					if record.CleanupBlockedAt != "2026-10-01T02:00:00Z" || record.CleanupRefusedSince != "2026-10-01T00:00:00Z" || record.DeferredCleanupBlocked == nil || record.DeferredCleanupBlocked.Name != "old-candidate" {
						t.Fatalf("lost blocked cleanup metadata: %+v", record)
					}
				} else {
					for _, want := range []string{"cleanup=blocked", "automatic retries stopped", "inspect resource old-candidate", "delete or restore", "crabbox stop"} {
						if !strings.Contains(out.String(), want) {
							t.Errorf("missing %q in %s", want, &out)
						}
					}
				}
			})
		}
	}
}

func TestLeaseAuditBlockedCleanup(t *testing.T) {
	audit := CoordinatorLeaseCloudAudit{CleanupBlockedAt: "2026-10-01T02:00:00Z", CleanupError: "identity changed"}
	if got := leaseAuditCleanupSummary(audit); got != "blocked since=2026-10-01T02:00:00Z error=identity changed" {
		t.Fatal(got)
	}
}
