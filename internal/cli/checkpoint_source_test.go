package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckpointSourceCoordinatorAbsenceRequiresExactReleaseReceipt(t *testing.T) {
	for _, state := range []string{"released", "stopped", "pending", "retained", "replacement", "missing"} {
		t.Run(state, func(t *testing.T) {
			lease := CoordinatorLease{ID: "cbx_abcdef123456", Provider: "aws", CloudID: "i-fixture", State: "released"}
			switch state {
			case "stopped":
				lease.State = "stopped"
			case "pending":
				lease.CleanupStartedAt = "2026-08-01T00:00:00Z"
			case "retained":
				value := false
				lease.ReleaseDeletesServer = &value
			case "replacement":
				lease.CloudID = "i-replacement"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/v1/leases/cbx_abcdef123456" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				if state == "missing" {
					w.WriteHeader(404)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
			}))
			defer server.Close()
			cfg := Config{Provider: "aws", Coordinator: server.URL, CoordToken: "fixture-token"}
			coord, _, err := newCoordinatorClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			b := &coordinatorLeaseBackend{cfg: cfg, coord: coord}
			absent, err := b.CheckpointSourceAbsent(context.Background(), CheckpointSourceRequest{LeaseID: lease.ID, Capture: NativeCheckpointCapture{SourceID: "i-fixture"}, Resource: NativeCheckpointResourceRequest{Image: NativeCheckpointImage{Provider: "aws"}}})
			if absent != (state == "released") || (err != nil) != (state == "missing" || state == "replacement") {
				t.Fatalf("absent=%v err=%v", absent, err)
			}
		})
	}
}
