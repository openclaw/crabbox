package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

func TestPrewarmCoordinatorGuestCleanupStaysBehindReleaseOwner(t *testing.T) {
	clearConfigEnv(t)
	logPath := installRecordingSSH(t, t.TempDir())
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	const id = "cbx_abcdef123456"
	var reads, releases atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			provider := "aws"
			if reads.Add(1) > 1 {
				provider = "external"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: id, Provider: provider, State: "active", Host: "192.0.2.70", SSHPort: "22", SSHUser: "crabbox", TargetOS: targetLinux}})
		} else {
			releases.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: id, Provider: "aws", State: "released"}})
		}
	}))
	defer server.Close()
	var stderr bytes.Buffer
	backend := coordinatorReleaseTestBackend(server, &stderr)
	cause := errors.New("prewarm probe failed")
	err := (App{Stdout: io.Discard, Stderr: &stderr}).runPrewarmPostWarmupStep(context.Background(), backend, backend.cfg, LeaseTarget{LeaseID: id}, "probe", func() error { return cause })
	if !errors.Is(err, cause) {
		t.Fatalf("step error=%v, want original failure", err)
	}
	ssh, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if reads.Load() != 2 || releases.Load() != 0 || len(ssh) != 0 {
		t.Fatalf("reads=%d release POSTs=%d guest SSH bytes=%d; want fresh owner rejection before guest cleanup/release; stderr=%s", reads.Load(), releases.Load(), len(ssh), stderr.String())
	}
}
