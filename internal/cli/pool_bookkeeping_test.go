package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type bookkeepingListBackend struct {
	list func(context.Context, ListRequest) (any, error)
}

func (b bookkeepingListBackend) Spec() ProviderSpec { return ProviderSpec{Name: "blacksmith-testbox"} }
func (b bookkeepingListBackend) ListJSON(ctx context.Context, req ListRequest) (any, error) {
	return b.list(ctx, req)
}

func TestExternalRunnerBookkeepingBlockedListBudget(t *testing.T) {
	clearConfigEnv(t)
	synctest.Test(t, func(t *testing.T) {
		// The outer deadline is a watchdog, not the bookkeeping budget.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var stderr bytes.Buffer
		calls := 0
		backend := bookkeepingListBackend{list: func(ctx context.Context, req ListRequest) (any, error) {
			calls++
			if !req.All {
				t.Error("bookkeeping must request complete inventory")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		started := time.Now()
		App{Stderr: &stderr}.syncExternalRunnersBestEffort(ctx, Config{Provider: "blacksmith-testbox", Coordinator: "http://127.0.0.1:1"}, backend)
		if elapsed := time.Since(started); elapsed != 5*time.Second {
			t.Errorf("blocked post-success inventory took %s; want one 5s bookkeeping budget", elapsed)
		}
		if calls != 1 || !strings.Contains(stderr.String(), "warning: external runner portal sync") {
			t.Fatalf("calls=%d stderr=%q", calls, stderr.String())
		}
	})
}

func bookkeepingCommandFixture(t *testing.T, name, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX child-process fixture")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return file
}

func TestExternalRunnerBookkeepingChildCancellation(t *testing.T) {
	clearConfigEnv(t)
	for _, stage := range []string{"actions", "token", "owner"} {
		t.Run(stage, func(t *testing.T) {
			// The grandchild retains the output pipe after CommandContext kills
			// its parent. A context alone does not bound os/exec's pipe drain.
			name := "gh"
			if stage == "owner" {
				name = "git"
				for _, key := range []string{"CRABBOX_OWNER", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
					t.Setenv(key, "")
				}
			}
			marker := filepath.Join(t.TempDir(), "started")
			command := bookkeepingCommandFixture(t, name, fmt.Sprintf("/bin/sleep 3 &\nprintf 'started\\n' >> %s\nwait\n", shellQuote(marker)))
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.WriteString(w, `{}`)
			}))
			defer server.Close()
			cfg := Config{Provider: "blacksmith-testbox", Coordinator: server.URL}
			rows := []CoordinatorExternalRunner{{ID: "tbx_ready", Repo: "example-org/my-app"}}
			if stage == "actions" {
				rows[0].Workflow = "ci.yml"
				rows = append(rows, CoordinatorExternalRunner{ID: "tbx_other", Repo: "example-org/my-app", Workflow: "other.yml"})
			} else if stage == "token" {
				cfg.CoordTokenCommand = []string{command}
			}
			backend := bookkeepingListBackend{list: func(context.Context, ListRequest) (any, error) { return rows, nil }}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			canceled := make(chan time.Time, 1)
			// Synchronize cancellation with the actual child, so a slow process
			// launch cannot make this pass without exercising inherited pipes.
			go func() {
				defer close(canceled)
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					if data, err := os.ReadFile(marker); err == nil && len(data) > 0 {
						canceled <- time.Now()
						cancel()
						return
					}
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
					}
				}
			}()
			var stderr bytes.Buffer
			App{Stderr: &stderr}.syncExternalRunnersBestEffort(ctx, cfg, backend)
			cancel()
			started := <-canceled
			if started.IsZero() {
				t.Fatal("fixture did not reach child cancellation")
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Errorf("%s cancellation waited %s for inherited pipes", stage, elapsed)
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "started\n" {
				t.Fatalf("repeated commands after cancellation: %q err=%v", data, err)
			}
			if requests.Load() != 0 || !strings.Contains(stderr.String(), "warning:") {
				t.Fatalf("requests=%d stderr=%q", requests.Load(), stderr.String())
			}
		})
	}
}

func TestExternalRunnerBookkeepingParentCancellation(t *testing.T) {
	clearConfigEnv(t)
	for _, mode := range []string{"already canceled", "short deadline", "late inventory"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				if mode == "already canceled" {
					cancel()
				}
				calls := 0
				backend := bookkeepingListBackend{list: func(ctx context.Context, _ ListRequest) (any, error) {
					calls++
					<-ctx.Done()
					if mode == "late inventory" {
						return []CoordinatorExternalRunner{{ID: "tbx_partial"}}, nil
					}
					return nil, ctx.Err()
				}}
				var stderr bytes.Buffer
				started := time.Now()
				App{Stderr: &stderr}.syncExternalRunnersBestEffort(ctx, Config{Provider: "blacksmith-testbox", Coordinator: "http://127.0.0.1:1"}, backend)
				if mode == "already canceled" && calls != 0 {
					t.Errorf("started inventory after cancellation")
				}
				if elapsed := time.Since(started); elapsed > 20*time.Millisecond {
					t.Errorf("parent cancellation took %s", elapsed)
				}
				if !strings.Contains(stderr.String(), "warning:") {
					t.Fatalf("stderr=%q", stderr.String())
				}
			})
		})
	}
}

func TestExternalRunnerBookkeepingNoopAndListFailure(t *testing.T) {
	clearConfigEnv(t)
	for _, mode := range []string{"no coordinator", "other provider", "no JSON capability", "list failure", "invalid inventory"} {
		t.Run(mode, func(t *testing.T) {
			cfg := Config{Provider: "blacksmith-testbox", Coordinator: "http://127.0.0.1:1"}
			if mode == "no coordinator" {
				cfg.Coordinator = ""
			}
			if mode == "other provider" {
				cfg.Provider = "ssh"
			}
			calls := 0
			var backend Backend = bookkeepingListBackend{list: func(context.Context, ListRequest) (any, error) {
				calls++
				if mode == "invalid inventory" {
					return make(chan int), nil
				}
				return nil, errors.New("inventory unavailable")
			}}
			if mode == "no JSON capability" {
				backend = testDelegatedBackend{spec: ProviderSpec{Name: "blacksmith-testbox"}}
			}
			var stderr bytes.Buffer
			App{Stderr: &stderr}.syncExternalRunnersBestEffort(context.Background(), cfg, backend)
			wantWarning := mode == "list failure" || mode == "invalid inventory"
			if strings.Contains(stderr.String(), "warning:") != wantWarning || (calls > 0) != wantWarning {
				t.Fatalf("calls=%d stderr=%q", calls, stderr.String())
			}
		})
	}
}

func TestExternalRunnerBookkeepingHTTPStalls(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_OWNER", "alice@example.com")
	for _, stage := range []string{"headers", "success body", "error body"} {
		t.Run(stage, func(t *testing.T) {
			var calls atomic.Int32
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if stage != "headers" {
					if stage == "error body" {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					_, _ = io.WriteString(w, `{`)
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer func() { close(release); server.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			var stderr bytes.Buffer
			backend := bookkeepingListBackend{list: func(context.Context, ListRequest) (any, error) {
				return []CoordinatorExternalRunner{{ID: "tbx_ready"}}, nil
			}}
			started := time.Now()
			App{Stderr: &stderr}.syncExternalRunnersBestEffort(ctx, Config{Provider: "blacksmith-testbox", Coordinator: server.URL}, backend)
			if time.Since(started) > time.Second || calls.Load() != 1 || !strings.Contains(stderr.String(), "warning:") {
				t.Fatalf("calls=%d elapsed=%s stderr=%q", calls.Load(), time.Since(started), stderr.String())
			}
		})
	}
}

func TestExternalRunnerBookkeepingOptionalEnrichment(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_OWNER", "alice@example.com")
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("BOOKKEEPING_CALLS", log)
	bookkeepingCommandFixture(t, "gh", `printf '%s\n' "$*" >> "$BOOKKEEPING_CALLS"
case "$*" in
  *missing.yml*) exit 1 ;;
  *) printf '[{"databaseId":123,"status":"in_progress","headBranch":"main","url":"https://github.com/example-org/my-app/actions/runs/123"}]' ;;
esac
`)
	rows := []CoordinatorExternalRunner{
		{ID: "tbx_one", Repo: "example-org/my-app", Workflow: "ci.yml", Ref: "main", Created: "2026-08-01T00:00:00Z"},
		{ID: "tbx_two", Repo: "example-org/my-app", Workflow: "ci.yml", Ref: "main"},
		{ID: "tbx_three", Repo: "example-org/my-app", Workflow: "missing.yml", Ref: "main"},
	}
	posted := make(chan []CoordinatorExternalRunner, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Provider string
			Runners  []CoordinatorExternalRunner
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Provider != "blacksmith-testbox" {
			t.Errorf("provider=%s", payload.Provider)
		}
		posted <- payload.Runners
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	backend := bookkeepingListBackend{list: func(context.Context, ListRequest) (any, error) { return rows, nil }}
	var stderr bytes.Buffer
	App{Stderr: &stderr}.syncExternalRunnersBestEffort(context.Background(), Config{Provider: "blacksmith-testbox", Coordinator: server.URL}, backend)
	select {
	case got := <-posted:
		if len(got) != 3 || got[1].ActionsRunID != "123" || got[2].ActionsRunID != "" || got[0].CreatedAt != rows[0].Created {
			t.Fatalf("payload=%+v", got)
		}
	default:
		t.Fatalf("no upload; stderr=%q", stderr.String())
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "--workflow") != 2 || stderr.Len() != 0 {
		t.Fatalf("calls=%s stderr=%q", data, stderr.String())
	}
}

func TestExternalRunnerBookkeepingUploadCancellation(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_OWNER", "alice@example.com")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Do not consume the large in-memory request body. Cancellation must
		// interrupt the native transport's write as well as response reads.
		cancel()
		<-release
	}))
	defer func() { close(release); server.Close() }()
	backend := bookkeepingListBackend{list: func(context.Context, ListRequest) (any, error) {
		return []CoordinatorExternalRunner{{ID: "tbx_ready", Status: strings.Repeat("x", 8<<20)}}, nil
	}}
	var stderr bytes.Buffer
	App{Stderr: &stderr}.syncExternalRunnersBestEffort(ctx, Config{Provider: "blacksmith-testbox", Coordinator: server.URL}, backend)
	if ctx.Err() != context.Canceled || calls.Load() != 1 || strings.Count(stderr.String(), "warning:") != 1 {
		t.Fatalf("context=%v calls=%d stderr=%q", ctx.Err(), calls.Load(), stderr.String())
	}
}
