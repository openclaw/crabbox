package blacksmith

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type warmupOutput struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *warmupOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
func (w *warmupOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestBlacksmithWarmupBookkeepingOutputOrder(t *testing.T) {
	for _, mode := range []string{"success", "sync failure", "canceled sync", "blocked list", "allocation failure", "no coordinator"} {
		t.Run(mode, func(t *testing.T) { testWarmupBookkeeping(t, mode) })
	}
}

func testWarmupBookkeeping(t *testing.T, mode string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX CLI fixtures")
	}
	home := t.TempDir()
	t.Chdir(home)
	for key, value := range map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": home, "XDG_STATE_HOME": home,
		"CRABBOX_CONFIG":            filepath.Join(home, "missing.yaml"),
		"CRABBOX_COORDINATOR_TOKEN": "", "CRABBOX_COORDINATOR_TOKEN_COMMAND": "",
		"CRABBOX_PROVIDER": "blacksmith-testbox", "CRABBOX_BLACKSMITH_ORG": "",
		"CRABBOX_ACCESS_CLIENT_ID": "", "CRABBOX_ACCESS_CLIENT_SECRET": "", "CRABBOX_ACCESS_TOKEN": "",
		"CF_ACCESS_CLIENT_ID": "", "CF_ACCESS_CLIENT_SECRET": "", "CF_ACCESS_TOKEN": "",
		"CRABBOX_COORDINATOR_MODE": "", "CRABBOX_OWNER": "alice@example.com",
	} {
		t.Setenv(key, value)
	}
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(home, "calls")
	t.Setenv("BOOKKEEPING_CALLS", log)
	t.Setenv("BOOKKEEPING_MODE", mode)
	marker := filepath.Join(home, "list-started")
	t.Setenv("BOOKKEEPING_STARTED", marker)
	for name, script := range map[string]string{
		"git":  "exit 1\n",
		"gh":   "exit 91\n",
		"curl": "exit 92\n",
		"blacksmith": `printf '%s\n' "$*" >> "$BOOKKEEPING_CALLS"
case "$*" in
  *"testbox warmup"*)
    if [ "$BOOKKEEPING_MODE" = 'allocation failure' ]; then exit 7; fi
    printf 'ready tbx_ready123\n' ;;
  *"testbox list"*)
    if [ "$BOOKKEEPING_MODE" = 'blocked list' ]; then
      /bin/sleep 3 &
      printf 'started' > "$BOOKKEEPING_STARTED"
      wait
      exit 1
    fi
    printf 'ID  STATUS  REPO  WORKFLOW  JOB  REF  CREATED\ntbx_ready123  ready  my-app  -  -  main  2026-08-01T00:00:00Z\n' ;;
  *) exit 93 ;;
esac
`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr warmupOutput
	var syncCalls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if mode == "blocked list" {
		done := make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				if _, err := os.Stat(marker); err == nil {
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
		defer func() { cancel(); <-done }()
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		syncCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.Contains(stdout.String(), "warmup complete") || strings.Contains(stderr.String(), `"totalMs"`) {
			t.Error("final warmup success/timing was emitted before portal bookkeeping finished")
		}
		if !strings.Contains(stdout.String(), "leased tbx_ready123 slug=ready-crab") {
			t.Errorf("ready lease identity missing before bookkeeping: %q", stdout.String())
		}
		if mode == "sync failure" {
			http.Error(w, "portal unavailable", http.StatusServiceUnavailable)
			return
		}
		if mode == "canceled sync" {
			cancel()
			return
		}
		_, _ = fmt.Fprintln(w, `{}`)
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	if mode == "no coordinator" {
		t.Setenv("CRABBOX_COORDINATOR", "")
	}
	err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(ctx, []string{
		"warmup", "--provider", "blacksmith-testbox", "--blacksmith-workflow", "ci.yml", "--slug", "ready-crab", "--keep", "--timing-json",
	})
	data, readErr := os.ReadFile(log)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Count(string(data), "testbox warmup") != 1 || strings.Contains(string(data), "testbox stop") {
		t.Fatalf("allocation repeated or ready lease stopped: %s", data)
	}
	if mode == "allocation failure" {
		var exitErr ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != 7 {
			t.Fatalf("allocation error=%v", err)
		}
		if syncCalls.Load() != 0 || strings.Contains(string(data), "testbox list") || strings.Contains(stdout.String(), "warmup complete") || strings.Contains(stderr.String(), `"totalMs"`) {
			t.Fatalf("allocation failure finalized or synchronized: stdout=%q stderr=%q calls=%s", stdout.String(), stderr.String(), data)
		}
		return
	}
	if err != nil {
		t.Fatalf("warmup: %v; stderr=%s", err, stderr.String())
	}
	wantCalls := int32(1)
	wantLists := 1
	if mode == "no coordinator" {
		wantCalls = 0
		wantLists = 0
	}
	if mode == "blocked list" {
		wantCalls = 0
	}
	if syncCalls.Load() != wantCalls || strings.Count(string(data), "testbox list") != wantLists {
		t.Fatalf("sync calls=%d provider calls=%s", syncCalls.Load(), data)
	}
	if strings.Count(stdout.String(), "warmup complete") != 1 {
		t.Fatalf("stdout=%q", stdout.String())
	}
	wantWarning := mode == "sync failure" || mode == "canceled sync" || mode == "blocked list"
	if strings.Contains(stdout.String(), "warning:") || strings.Contains(stderr.String(), "warning:") != wantWarning {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if strings.Count(stderr.String(), `"totalMs"`) != 1 {
		t.Fatalf("timing emitted more than once: %q", stderr.String())
	}
	var report core.TimingReport
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
		t.Fatalf("timing: %v; stderr=%q", err, stderr.String())
	}
	if report.LeaseID != "tbx_ready123" || report.Slug != "ready-crab" || report.ExitCode != 0 {
		t.Fatalf("timing=%+v", report)
	}
	claim, err := readLeaseClaim("tbx_ready123")
	if err != nil || claim.Slug != "ready-crab" {
		t.Fatalf("retained claim=%+v err=%v", claim, err)
	}
	key, err := testboxKeyPath("tbx_ready123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("retained key missing: %v", err)
	}
}

type bookkeepingClock struct{ now time.Time }

func (c *bookkeepingClock) Now() time.Time { return c.now }

type bookkeepingFailWriter struct{}

func (bookkeepingFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestBlacksmithWarmupBookkeepingFinalization(t *testing.T) {
	for _, mode := range []string{"success", "canceled bookkeeping", "timing writer failure", "allocation failure"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", home)
			t.Setenv("XDG_STATE_HOME", home)
			clock := &bookkeepingClock{now: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
			var stdout, stderr bytes.Buffer
			runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
				if !strings.Contains(strings.Join(req.Args, " "), "testbox warmup") {
					t.Fatalf("unexpected provider operation: %v", req.Args)
				}
				clock.now = clock.now.Add(time.Second)
				if mode == "allocation failure" {
					return LocalCommandResult{ExitCode: 7}, errors.New("allocation rejected")
				}
				return LocalCommandResult{Stdout: "ready tbx_ready123\n"}, nil
			}}
			cfg := baseConfig()
			cfg.Blacksmith.Workflow = "ci.yml"
			backend := newTestBlacksmithBackend(cfg, runner)
			backend.rt.Clock, backend.rt.Stdout, backend.rt.Stderr = clock, &stdout, &stderr
			if mode == "timing writer failure" {
				backend.rt.Stderr = bookkeepingFailWriter{}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finalizations := 0
			err := backend.Warmup(ctx, WarmupRequest{
				Repo: Repo{Root: home}, RequestedSlug: "ready-crab", Keep: true, TimingJSON: true,
				BeforeComplete: func() {
					finalizations++
					if !strings.Contains(stdout.String(), "leased tbx_ready123 slug=ready-crab") || strings.Contains(stdout.String(), "warmup complete") || stderr.Len() != 0 {
						t.Fatalf("premature final output: stdout=%q stderr=%q", stdout.String(), stderr.String())
					}
					if _, err := readLeaseClaim("tbx_ready123"); err != nil {
						t.Fatalf("lease not retained before finalization: %v", err)
					}
					clock.now = clock.now.Add(4 * time.Second)
					if mode == "canceled bookkeeping" {
						cancel()
					}
				},
			})
			if mode == "allocation failure" {
				if err == nil || finalizations != 0 || strings.Contains(stdout.String(), "warmup complete") || stderr.Len() != 0 {
					t.Fatalf("err=%v finalizations=%d stdout=%q stderr=%q", err, finalizations, stdout.String(), stderr.String())
				}
				return
			}
			if mode == "timing writer failure" {
				if !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("timing writer err=%v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				var report core.TimingReport
				if err := json.Unmarshal(stderr.Bytes(), &report); err != nil {
					t.Fatal(err)
				}
				if report.TotalMs != 5000 || report.ExitCode != 0 || report.LeaseID != "tbx_ready123" || report.Slug != "ready-crab" {
					t.Fatalf("timing=%+v", report)
				}
			}
			if finalizations != 1 || len(runner.calls) != 1 || strings.Count(stdout.String(), "warmup complete total=5s") != 1 {
				t.Fatalf("finalizations=%d calls=%v stdout=%q", finalizations, runner.calls, stdout.String())
			}
			if _, err := readLeaseClaim("tbx_ready123"); err != nil {
				t.Fatalf("claim lost: %v", err)
			}
		})
	}
}
