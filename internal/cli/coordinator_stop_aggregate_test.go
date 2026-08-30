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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorStopAggregateGuestCleanupPreservesReleaseBudget(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		name := "responsive hydrated guest"
		if stalled {
			name = "stalled guest hygiene"
		}
		t.Run(name, func(t *testing.T) { testCoordinatorStopAggregateGuestCleanup(t, stalled) })
	}
}

func testCoordinatorStopAggregateGuestCleanup(t *testing.T, stalled bool) {
	clearConfigEnv(t)
	dir := t.TempDir()
	logPath := installRecordingSSH(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	script, err := os.ReadFile(sshPath)
	if err != nil {
		t.Fatal(err)
	}
	// Real CLI/SSH execution, with guest responses and delays confined to the fixture.
	script = []byte(strings.Replace(string(script), `if [ -n "${CRABBOX_FAKE_SSH_STDIN_LOG:-}" ]; then`, `case "$match" in
 *"cat "*".crabbox/actions/"*".env"*) printf 'WORKSPACE=/tmp/fixture-workspace\n'; exit 0 ;;
 *"touch "*".crabbox/actions/"*".stop"*) exit 0 ;;
 *"pkill -f"*) exec /bin/sleep 30 ;;
 *"tailscale logout"*) exec /bin/sleep 30 ;;
esac
if [ -n "${CRABBOX_FAKE_SSH_STDIN_LOG:-}" ]; then`, 1))
	if !stalled {
		script = []byte(strings.ReplaceAll(string(script), "exec /bin/sleep 30", "exit 0"))
	}
	if err := os.WriteFile(sshPath, script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	const id = "cbx_abcdef123456"
	started := time.Now()
	var reads, releases atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+id:
			reads.Add(1)
			t.Logf("lease read at %s", time.Since(started))
			if stalled {
				if err := sleepContext(r.Context(), 4*time.Second); err != nil {
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
				ID: id, Provider: "aws", State: "active", Host: "192.0.2.70", SSHPort: "22", SSHUser: "crabbox", TargetOS: targetLinux,
				Tailscale: &TailscaleMetadata{Enabled: true},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+id+"/release":
			releases.Add(1)
			t.Logf("release POST at %s", time.Since(started))
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["delete"] != true || body["expectedProvider"] != "aws" {
				t.Errorf("invalid release body=%v err=%v", body, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: id, Provider: "aws", State: "released"}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "synthetic-stop-token")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	err = (App{Stdout: io.Discard, Stderr: &stderr}).stop(ctx, []string{"--provider", "aws", "--id", id})
	log, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, command := range []string{".env", ".stop", "pkill -f", "tailscale logout"} {
		if !bytes.Contains(log, []byte(command)) {
			t.Errorf("guest phase %q was not attempted", command)
		}
	}
	t.Logf("elapsed=%s GETs=%d release POSTs=%d parent=%v stderr=%s", time.Since(started), reads.Load(), releases.Load(), ctx.Err(), stderr.String())
	if !stalled && time.Since(started) < 20*time.Second {
		t.Error("responsive hydrated guest lost its 20-second stop-marker grace")
	}
	if err != nil || releases.Load() != 1 || ctx.Err() != nil {
		t.Fatalf("stop err=%v release POSTs=%d parent=%v; want one authoritative release before caller deadline", err, releases.Load(), ctx.Err())
	}
}

func TestHydrationStopGraceHonorsCallerCancellation(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	logPath := installRecordingSSH(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	script, err := os.ReadFile(sshPath)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte(strings.Replace(string(script), `if [ -n "${CRABBOX_FAKE_SSH_STDIN_LOG:-}" ]; then`, `case "$match" in
 *"cat "*".crabbox/actions/"*".env"*) printf 'WORKSPACE=/tmp/fixture-workspace\n'; exit 0 ;;
esac
if [ -n "${CRABBOX_FAKE_SSH_STDIN_LOG:-}" ]; then`, 1))
	if err := os.WriteFile(sshPath, script, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	started := time.Now()
	var stderr bytes.Buffer
	(App{Stderr: &stderr}).writeActionsHydrationStopBestEffort(ctx, SSHTarget{Host: "192.0.2.70", Port: "22", User: "crabbox", TargetOS: targetLinux}, "cbx_abcdef123456")
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(log, []byte(".env")) || !bytes.Contains(log, []byte("touch ")) {
		t.Fatal("hydration read and stop write did not run")
	}
	if strings.Contains(stderr.String(), "could not stop GitHub Actions hydration") {
		t.Fatalf("marker write failed before the grace period: %s", stderr.String())
	}
	if ctx.Err() != context.DeadlineExceeded || time.Since(started) > 3*time.Second {
		t.Fatalf("hydration cleanup elapsed=%s parent=%v, want prompt cancellation after successful marker", time.Since(started), ctx.Err())
	}
}
