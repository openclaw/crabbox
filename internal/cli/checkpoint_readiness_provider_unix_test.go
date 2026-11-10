//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise checkpointFork and the shipped Machine0/local-container acquisition
// callers, not a test backend that supplies a chosen diagnostic phase. The
// fixtures have no real provider access; .invalid prevents SSH guest contact.
func TestCheckpointReadinessProviderAcquisitionBudget(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary, err := builtCLITestBinary()
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-trimpath", "-o", binary+".provider", "./internal/cli/testdata/checkpoint-provider")
	build.Dir = repo
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build provider fixture: %v\n%s", err, output)
	}
	docker := buildCheckpointContainerRuntime(t, repo)
	for _, provider := range []string{"machine0", "local-container"} {
		t.Run(provider, func(t *testing.T) {
			var f *checkpointCaptureFixture
			var checkpointID, resourceID string
			if provider == "machine0" {
				f = newCheckpointCaptureFixture(t, repo, binary)
				state := f.state()
				state.Ready = true
				f.writeState(state)
				f.finishRetirement()
				state = f.state()
				state.ForkIP = "checkpoint-readiness.invalid"
				f.writeState(state)
				checkpointID, resourceID = captureFixtureCheckpoint, "fixture-fork-immutable-id"
			} else {
				f = newCheckpointContainerFixture(t, repo, binary, docker)
				result := f.run("checkpoint", "create", "--provider", provider, "--id", captureFixtureLease, "--mode", "native", "--json")
				if result.err != nil {
					t.Fatalf("capture: %v\n%s", result.err, result.stderr)
				}
				var record checkpointRecord
				if err := json.Unmarshal(result.stdout, &record); err != nil {
					t.Fatalf("capture JSON: %v\n%s", err, result.stdout)
				}
				checkpointID, resourceID = record.ID, strings.Repeat("b", 64)
				var state captureContainerState
				f.readJSON(filepath.Join(f.root, "docker.json"), &state)
				state.Removed, state.ForkHost = true, "checkpoint-readiness.invalid"
				f.writeJSON(filepath.Join(f.root, "docker.json"), state)
			}
			const leaseID = "cbx_123456abcdef"
			for _, budget := range []string{"100ms", "200ms"} {
				result := f.run("checkpoint", "fork", checkpointID, "--lease-id", leaseID, "--slug", "wait-budget", "--keep", "--wait-timeout", budget, "--json")
				var exit *exec.ExitError
				if !errors.As(result.err, &exit) || exit.ExitCode() != 5 || len(result.stdout) != 0 || !bytes.Contains(result.stderr, []byte("state=transport_failure")) || !bytes.Contains(result.stderr, []byte("lease="+leaseID)) {
					t.Fatalf("budget=%s err=%v stdout=%s stderr=%s", budget, result.err, result.stdout, result.stderr)
				}
				var claim leaseClaim
				f.readJSON(filepath.Join(f.root, "state", "crabbox", "claims", leaseID+".json"), &claim)
				if claim.CloudID != resourceID || claim.FixedCreateIntent == nil || claim.FixedCreateIntent.CheckpointID != checkpointID || claim.FixedCreateIntent.State == "released" {
					t.Fatalf("timeout lost fixed identity: %+v", claim)
				}
				var creates int
				if provider == "machine0" {
					creates = f.state().Creates
				} else {
					var state captureContainerState
					f.readJSON(filepath.Join(f.root, "docker.json"), &state)
					creates = state.Creates
				}
				if creates != 1 {
					t.Fatalf("budget=%s created %d resources; want same resource on replay", budget, creates)
				}
				t.Logf("budget=%s: %s", budget, result.stderr)
			}
		})
	}
}
