//go:build !windows

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

type coordinatorPrepareScriptBackend struct {
	*sshScriptTestBackend
	observation *coordinatorLeaseBackend
}

func (b *coordinatorPrepareScriptBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	if _, err := b.observation.Resolve(ctx, req); err != nil {
		return LeaseTarget{}, err
	}
	return b.sshScriptTestBackend.Resolve(ctx, req)
}

func TestCoordinatorPrepareRecoveryPrecedesScriptAndDoesNotReplayFailure(t *testing.T) {
	p, b, dir := setupSSHScriptRun(t)
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/v1/leases/"+b.lease.LeaseID {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if calls == 1 {
			http.Error(w, "temporary", 500)
			return
		}
		close(entered)
		<-release
		json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: b.lease.LeaseID, Provider: p.Name(), State: "active"}})
	}))
	defer server.Close()
	p.backend = &coordinatorPrepareScriptBackend{sshScriptTestBackend: b, observation: &coordinatorLeaseBackend{cfg: Config{Provider: p.Name()}, coord: &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}}}
	marker := filepath.Join(dir, "script-executions")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- (App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("printf x >> " + shellQuote(marker) + "\nexit 23\n")}).runCommand(ctx, []string{"--provider", p.Name(), "--id", b.lease.LeaseID, "--no-sync", "--no-hydrate", "--keep", "--script-stdin"})
	}()
	joined, released := false, false
	defer func() {
		if !released {
			close(release)
		}
		cancel()
		if !joined {
			<-done
		}
	}()
	select {
	case <-entered:
	case err := <-done:
		joined = true
		t.Fatalf("run ended before second observation: %v", err)
	}
	for _, path := range []string{filepath.Join(dir, "ssh.log"), b.activityPath, marker} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("execution preceded lease observation: %s error=%v", filepath.Base(path), err)
		}
	}
	close(release)
	released = true
	err := <-done
	joined = true
	if ExitCodeForError(err, 0) != 23 {
		t.Fatalf("run error=%v stderr=%s", err, stderr.String())
	}
	body, readErr := os.ReadFile(marker)
	if readErr != nil || string(body) != "x" || calls != 2 || b.starts != 1 || b.joined != 1 {
		t.Fatalf("script=%q read=%v GETs=%d activity=%d/%d", body, readErr, calls, b.starts, b.joined)
	}
}
