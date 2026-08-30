package cli

import (
	"context"
	"encoding/json"
	"github.com/gofrs/flock"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestCheckpointCoordinatorReleaseHoldsClaimFenceThroughMutation(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "retained"}[retained], func(t *testing.T) {
			isolateTestUserDirs(t)
			const id = "cbx_abcdef123456"
			if err := claimLeaseTargetForConfig(id, "capture-fence", Config{Provider: "aws"}, Server{Provider: "aws", CloudID: "i-fixture"}, SSHTarget{}, time.Hour); err != nil {
				t.Fatal(err)
			}
			path, err := leaseClaimPath(id)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lockPath, err := leaseClaimLockPath(path)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				lock := flock.New(lockPath)
				acquired, err := lock.TryLock()
				if acquired {
					_ = lock.Unlock()
				}
				if err != nil || acquired {
					t.Errorf("release POST did not hold durable claim fence: acquired=%t err=%v", acquired, err)
				}
				deletes := !retained
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: id, Provider: "aws", CloudID: "i-fixture", State: "released", ReleaseDeletesServer: &deletes}})
			}))
			defer server.Close()
			b := coordinatorReleaseTestBackend(server, io.Discard)
			if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: id, Server: Server{Provider: "aws"}}}); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("release calls=%d", calls)
			}
			after, err := os.ReadFile(path)
			if retained {
				if err != nil || string(after) != string(before) {
					t.Fatal("retained release changed its claim")
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("confirmed release left claim: %v", err)
			}
			if _, err := os.Stat(filepath.Dir(lockPath)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCheckpointSourceCoordinatorUsesAdminReceiptFallback(t *testing.T) {
	for _, primaryStatus := range []int{401, 404, 500} {
		t.Run(http.StatusText(primaryStatus), func(t *testing.T) {
			isolateTestUserDirs(t)
			const id = "cbx_abcdef123456"
			adminReads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/v1/leases/"+id {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				if r.Header.Get("Authorization") != "Bearer fixture-admin" {
					w.WriteHeader(primaryStatus)
					return
				}
				adminReads++
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: id, Provider: "aws", CloudID: "i-fixture", State: "released"}})
			}))
			defer server.Close()
			cfg := Config{Provider: "aws", Coordinator: server.URL, CoordToken: "fixture-user", CoordAdminToken: "fixture-admin"}
			coord, _, err := newCoordinatorClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			b := &coordinatorLeaseBackend{cfg: cfg, coord: coord}
			absent, err := b.CheckpointSourceAbsent(context.Background(), CheckpointSourceRequest{LeaseID: id, Capture: NativeCheckpointCapture{SourceID: "i-fixture"}, Resource: NativeCheckpointResourceRequest{Image: NativeCheckpointImage{Provider: "aws"}}})
			fallback := primaryStatus != 500
			if absent != fallback || (err == nil) != fallback || (adminReads == 1) != fallback {
				t.Fatalf("absent=%t err=%v adminReads=%d", absent, err, adminReads)
			}
		})
	}
}
