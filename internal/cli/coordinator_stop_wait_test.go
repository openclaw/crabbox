package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorStopWaitPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, flag        string
		pending, rejected bool
	}{
		{name: "default waits"},
		{name: "explicit wait", flag: "--wait=true"},
		{name: "accepted without wait", flag: "--wait=false", pending: true},
		{name: "rejection is not acceptance", flag: "--wait=false", rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			configureCoordinatorReleaseTestTiming(t, time.Second, 0)
			const id = "cbx_abcdef123456"
			keyPath, claimPath := managedStopLocalState(t, id)
			claimBefore, err := os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			var posts, observations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				lease := CoordinatorLease{ID: id, Provider: "aws", State: "active", TargetOS: targetLinux}
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+id+"/release":
					posts.Add(1)
					if tc.rejected {
						http.Error(w, "forbidden", http.StatusForbidden)
						return
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["delete"] != true || body["expectedProvider"] != "aws" {
						t.Errorf("release body=%v err=%v", body, err)
					}
					deletes := true
					lease.State, lease.ReleaseDeletesServer, lease.CleanupStartedAt = "released", &deletes, "2026-10-12T00:00:00Z"
				case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+id:
					if posts.Load() > 0 {
						observations.Add(1)
						lease = confirmedCoordinatorRelease(id, "aws")
					}
				default:
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "synthetic-stop-token")
			args := []string{"--provider", "aws", "--id", id}
			if tc.flag != "" {
				args = append(args, tc.flag)
			}
			var stderr bytes.Buffer
			err = (App{Stdout: io.Discard, Stderr: &stderr}).stop(t.Context(), args)
			if (err != nil) != tc.rejected {
				t.Fatalf("stop=%v stderr=%s", err, stderr.String())
			}
			if tc.pending || tc.rejected {
				if observations.Load() != 0 {
					t.Fatal("non-waiting stop polled completion")
				}
				if data, err := os.ReadFile(claimPath); err != nil || !bytes.Equal(data, claimBefore) {
					t.Fatalf("pending claim changed: %v", err)
				}
				if data, err := os.ReadFile(keyPath); err != nil || string(data) != "private" {
					t.Fatalf("pending key changed: %v", err)
				}
				if tc.pending && !strings.Contains(stderr.String(), "release accepted") {
					t.Fatalf("missing acceptance: %s", stderr.String())
				}
			} else {
				if observations.Load() != 1 {
					t.Fatalf("observations=%d", observations.Load())
				}
				if _, err := os.Stat(claimPath); !os.IsNotExist(err) {
					t.Fatalf("confirmed claim retained: %v", err)
				}
				if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
					t.Fatalf("confirmed key retained: %v", err)
				}
			}
		})
	}
}
