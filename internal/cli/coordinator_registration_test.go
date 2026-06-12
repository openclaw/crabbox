package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRegisterCoordinatorLeaseBestEffortMapsDirectLease(t *testing.T) {
	var got CoordinatorLeaseRegistration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"external","lifecycle":"registered","state":"active"}}`))
	}))
	defer server.Close()

	var stderr bytes.Buffer
	cfg := baseConfig()
	cfg.Provider = "external"
	cfg.Coordinator = server.URL
	cfg.CoordToken = "token"
	cfg.BrokerMode = BrokerModeRegistered
	cfg.Desktop = true
	cfg.DesktopEnv = "gnome"
	cfg.WorkRoot = "/workspace"
	cfg.ExposedPorts = []string{"3000", "8080"}
	cfg.TTL = 2 * time.Hour
	cfg.IdleTimeout = 30 * time.Minute
	app := App{Stderr: &stderr}
	lease := LeaseTarget{
		LeaseID: "cbx_123",
		Server: Server{
			Provider: "external",
			CloudID:  "external-box-123",
			Name:     "my-box",
		},
		SSH: SSHTarget{Host: "192.0.2.10", User: "runner", Port: "22", TargetOS: targetLinux},
	}
	lease.Server.ServerType.Name = "cpu16"
	app.registerCoordinatorLeaseBestEffort(context.Background(), cfg, lease)
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if got.Provider != "external" || got.CloudID != "external-box-123" || got.Host != "192.0.2.10" || got.WorkRoot != "/workspace" || !got.Desktop || got.DesktopEnv != "gnome" || len(got.ExposedPorts) != 2 || got.TTLSeconds != 7200 || got.IdleTimeoutSeconds != 1800 {
		t.Fatalf("registration=%#v", got)
	}
}

func TestReleaseRegisteredCoordinatorLeaseNeverRequestsProviderDeletion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var got struct {
		Delete bool `json:"delete"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_123/release" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"external","lifecycle":"registered","state":"released"}}`))
	}))
	defer server.Close()

	var stderr bytes.Buffer
	app := App{Stdout: &bytes.Buffer{}, Stderr: &stderr}
	app.releaseRegisteredCoordinatorLeaseBestEffort(context.Background(), Config{
		Coordinator: server.URL,
		CoordToken:  "token",
		BrokerMode:  BrokerModeRegistered,
	}, "cbx_123")
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if got.Delete {
		t.Fatal("registered coordinator release requested provider deletion")
	}
}
