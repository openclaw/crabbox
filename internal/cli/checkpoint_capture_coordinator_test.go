//go:build !windows

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func runCheckpointCoordinatorNonSubmissionContract(t *testing.T, repo, binary string) {
	for _, phase := range []string{"preparation", "submission", "legacy-submission"} {
		t.Run(phase, func(t *testing.T) {
			f := newCheckpointCaptureFixture(t, repo, binary)
			const instanceID = "i-0123456789abcdef0"
			var creates atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+captureFixtureLease:
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
						ID: captureFixtureLease, Slug: "checkpoint-proof", Provider: "aws", TargetOS: targetLinux,
						CloudID: instanceID, Host: "127.0.0.1", SSHUser: "crabbox", SSHPort: "22", State: "active",
					}})
				case r.URL.Path == "/v1/checkpoints" || r.URL.Path == "/v1/images":
					if r.Method == http.MethodPost {
						creates.Add(1)
					}
					if phase == "legacy-submission" && r.URL.Path == "/v1/checkpoints" {
						http.NotFound(w, r)
						return
					}
					w.WriteHeader(http.StatusBadGateway)
					fmt.Fprint(w, `{"error":"fixture checkpoint submission uncertain"}`)
				default:
					t.Errorf("unexpected coordinator request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(endpoint.Close)
			config := "provider: aws\nnetwork: public\ncoordinator: " + endpoint.URL + "\naws:\n  region: eu-west-1\ntargetOS: linux\n"
			if err := os.WriteFile(filepath.Join(f.root, "config.yaml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			prepared := filepath.Join(f.root, "prepared-source")
			sshFixture := `#!/bin/sh
for arg do remote=$arg; done
case "$remote" in
  'exit 0') exit 0;;
  ` + shellQuote("bash -lc "+shellQuote(remotePrepareNativeImageCommand())) + `) printf 'prepare\n' >> "$CAPTURE_COORDINATOR_PREPARED"; if [ "$CAPTURE_COORDINATOR_FAILURE" = preparation ]; then echo 'fixture pre-clean failure' >&2; exit 1; fi;;
  *) echo 'unexpected SSH command' >&2; exit 97;;
esac
`
			if err := os.WriteFile(filepath.Join(f.root, "bin", "ssh"), []byte(sshFixture), 0o700); err != nil {
				t.Fatal(err)
			}
			f.env = append(f.env, "CRABBOX_COORDINATOR_TOKEN=fixture", "CRABBOX_COORDINATOR_ADMIN_TOKEN=fixture-admin", "CAPTURE_COORDINATOR_FAILURE="+phase, "CAPTURE_COORDINATOR_PREPARED="+prepared)
			claim := f.claim()
			claim.Provider, claim.CloudID, claim.CloudImmutableID, claim.ProviderScope, claim.Slug = "aws", instanceID, "", "", "checkpoint-proof"
			claim.FixedCreateIntent, claim.Labels = nil, nil
			f.writeJSON(filepath.Join(f.root, "state", "crabbox", "claims", captureFixtureLease+".json"), claim)
			result := f.run("checkpoint", "create", "--provider", "aws", "--id", captureFixtureLease, "--mode", "native", "--strategy", "image", "--wait=false", "--json")
			if result.err == nil {
				t.Fatal("fixture failure unexpectedly succeeded")
			}
			if calls, err := os.ReadFile(prepared); err != nil || string(calls) != "prepare\n" {
				t.Fatalf("expected one source preparation: calls=%q err=%v stderr=%s", calls, err, result.stderr)
			}
			records, err := filepath.Glob(filepath.Join(f.root, "state", "crabbox", "checkpoints", "*", checkpointMetaFile))
			if err != nil {
				t.Fatal(err)
			}
			if phase != "preparation" {
				wantCreates := int32(1)
				if phase == "legacy-submission" {
					wantCreates = 2
				}
				if creates.Load() != wantCreates || len(records) != 1 || len(result.stdout) != 0 {
					t.Fatalf("uncertain submission lost reservation: creates=%d records=%v stdout=%s stderr=%s", creates.Load(), records, result.stdout, result.stderr)
				}
				var record checkpointRecord
				f.readJSON(records[0], &record)
				if record.coordinatorManaged() != (phase == "submission") || record.Native.ImageID != "" {
					t.Fatalf("uncertain submission changed ownership: %+v", record)
				}
				return
			}
			var outcome map[string]string
			if err := json.Unmarshal(result.stdout, &outcome); err != nil || len(outcome) != 6 || outcome["schema"] != "crabbox.checkpoint.create.failure.v1" || outcome["outcome"] != "not_submitted" || outcome["provider"] != "aws" || outcome["leaseId"] != captureFixtureLease || outcome["localReservation"] != "removed" || !strings.HasPrefix(outcome["checkpointId"], checkpointIDPrefix) {
				t.Fatalf("missing non-submission receipt: outcome=%v parse=%v stderr=%s", outcome, err, result.stderr)
			}
			if creates.Load() != 0 || len(records) != 0 || !strings.Contains(string(result.stderr), "fixture pre-clean failure") {
				t.Fatalf("preparation failure crossed submission or lost cause: creates=%d records=%v stderr=%s", creates.Load(), records, result.stderr)
			}
			if _, err := os.Lstat(filepath.Join(f.root, "state", "crabbox", "checkpoints", outcome["checkpointId"])); !os.IsNotExist(err) {
				t.Fatalf("reservation directory remains: %v", err)
			}
		})
	}
}
