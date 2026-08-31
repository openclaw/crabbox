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
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorReleaseCancelsClaimFenceWait(t *testing.T) {
	clearConfigEnv(t)
	configureCoordinatorReleaseTestTiming(t, time.Second, 0)
	const id = "cbx_abcdef123456"
	keyPath, claimPath := managedStopLocalState(t, id)
	claimBefore, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}
	var requests, cleanups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected release after cancellation", http.StatusInternalServerError)
	}))
	defer server.Close()
	backend := coordinatorReleaseTestBackend(server, io.Discard)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	var releaseErr error
	settledWhileHeld := false
	err = withLeaseClaimLockContext(t.Context(), claimPath, true, func() error {
		go func() {
			done <- backend.ReleaseLease(ctx, ReleaseLeaseRequest{
				Lease:                LeaseTarget{LeaseID: id, Server: Server{Provider: "aws"}, SSH: SSHTarget{Host: "192.0.2.70"}},
				GuardedRemoteCleanup: func(context.Context, LeaseTarget) { cleanups.Add(1) },
			})
		}()
		waitClaimWriter(t, claimPath)
		cancel()
		select {
		case releaseErr = <-done:
			settledWhileHeld = true
		case <-time.After(time.Second):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !settledWhileHeld {
		releaseErr = <-done
	}
	if !settledWhileHeld || !errors.Is(releaseErr, context.Canceled) {
		t.Errorf("release settled while claim fence held=%v error=%v; want canceled before unlock", settledWhileHeld, releaseErr)
	}
	if requests.Load() != 0 || cleanups.Load() != 0 {
		t.Errorf("requests=%d guest cleanups=%d; canceled fence must not authorize remote work", requests.Load(), cleanups.Load())
	}
	if after, err := os.ReadFile(claimPath); err != nil || !bytes.Equal(after, claimBefore) {
		t.Errorf("canceled fence changed exact claim: %v", err)
	}
	if key, err := os.ReadFile(keyPath); err != nil || string(key) != "private" {
		t.Errorf("canceled fence changed SSH artifact: %v", err)
	}
}

func TestCoordinatorStopSharesCompletionBudget(t *testing.T) {
	for _, phase := range []string{"initial inspection", "fresh inspection", "pending observation", "completed observation", "egress fence"} {
		t.Run(phase, func(t *testing.T) {
			clearConfigEnv(t)
			budget, observationDelay := 100*time.Millisecond, time.Duration(0)
			if strings.Contains(phase, "observation") {
				budget, observationDelay = 2*time.Second, 1500*time.Millisecond
			}
			configureCoordinatorReleaseTestTiming(t, budget, 0)
			const id = "cbx_abcdef123456"
			keyPath, claimPath := managedStopLocalState(t, id)
			claimBefore, err := os.ReadFile(claimPath)
			if err != nil {
				t.Fatal(err)
			}
			var unlock func()
			if phase == "egress fence" {
				unlock, err = acquireEgressDaemonLock(t.Context(), id)
			}
			if err != nil {
				t.Fatal(err)
			}
			if unlock != nil {
				defer func() {
					if unlock != nil {
						unlock()
					}
				}()
			}
			var reads, posts atomic.Int32
			entered := make(chan struct{})
			held := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				lease := CoordinatorLease{ID: id, Provider: "aws", State: "released", TargetOS: targetLinux}
				if r.Method == http.MethodPost {
					posts.Add(1)
					if strings.Contains(phase, "observation") {
						lease.CleanupStartedAt = "2026-08-19T00:00:00Z"
					}
				} else if r.Method == http.MethodGet {
					read := reads.Add(1)
					if read == 1 {
						close(entered)
						if err := sleepContext(r.Context(), observationDelay); err != nil {
							return
						}
					}
					if phase == "fresh inspection" && read == 1 {
						lease.State, lease.Host = "active", "192.0.2.70"
					}
					if (phase == "initial inspection" && read == 1) ||
						(phase == "fresh inspection" && read == 2) ||
						(phase == "pending observation" && posts.Load() == 1) {
						select {
						case <-r.Context().Done():
							return
						case <-held:
						}
					}
				} else {
					t.Errorf("unexpected method %s", r.Method)
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
			}))
			defer server.Close()
			t.Setenv("CRABBOX_COORDINATOR", server.URL)
			t.Setenv("CRABBOX_COORDINATOR_TOKEN", "synthetic-stop-token")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var stderr bytes.Buffer
			done := make(chan error, 1)
			args := []string{"--provider", "aws", "--id", id}
			go func() {
				done <- (App{Stdout: io.Discard, Stderr: &stderr}).stop(ctx, args)
			}()
			select {
			case <-entered:
			case err = <-done:
				close(held)
				t.Fatalf("stop ended before initial inspection: %v", err)
			}
			settledWithinBudget := false
			select {
			case err = <-done:
				settledWithinBudget = true
			case <-time.After(budget + time.Second):
			}
			cancel()
			close(held)
			if unlock != nil {
				unlock()
				unlock = nil
			}
			if !settledWithinBudget {
				err = <-done
			}
			confirmed := phase == "completed observation" || strings.HasSuffix(phase, "fence")
			if !settledWithinBudget {
				t.Errorf("stop did not settle within shared completion budget while %s remained held", phase)
			}
			if confirmed {
				if err != nil || posts.Load() != 1 {
					t.Errorf("confirmed stop error=%v POSTs=%d", err, posts.Load())
				}
				for _, path := range []string{keyPath, claimPath} {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("confirmed release retained %s: %v", filepath.Base(path), err)
					}
				}
				if strings.HasSuffix(phase, "fence") && !strings.Contains(stderr.String(), "deadline exceeded") {
					t.Errorf("canceled local cleanup must warn without changing confirmed provider result: %s", stderr.String())
				}
			} else {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("error=%v; want shared completion deadline", err)
				}
				wantPosts := int32(0)
				if phase == "pending observation" {
					wantPosts = 1
				}
				if posts.Load() != wantPosts {
					t.Errorf("release POSTs=%d want=%d", posts.Load(), wantPosts)
				}
				if after, readErr := os.ReadFile(claimPath); readErr != nil || !bytes.Equal(after, claimBefore) {
					t.Errorf("unconfirmed release changed claim: %v", readErr)
				}
				if key, readErr := os.ReadFile(keyPath); readErr != nil || string(key) != "private" {
					t.Errorf("unconfirmed release changed SSH artifact: %v", readErr)
				}
			}
		})
	}
}

func TestWebVNCDaemonStopCancelsFenceWait(t *testing.T) {
	clearConfigEnv(t)
	const id = "workspace-stop-lifetime"
	unlock, err := acquireWebVNCDaemonLock(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- (App{Stdout: io.Discard, Stderr: io.Discard}).webVNCDaemonCommand(ctx, []string{"stop", "--id", id})
	}()
	settledWhileHeld := false
	select {
	case err = <-done:
		settledWhileHeld = true
	case <-time.After(time.Second):
	}
	unlock()
	unlock = nil
	if !settledWhileHeld {
		err = <-done
	}
	if !settledWhileHeld || err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("daemon stop settled while fence held=%v error=%v; want canceled before unlock", settledWhileHeld, err)
	}
}

func managedStopLocalState(t *testing.T, id string) (string, string) {
	t.Helper()
	keyPath, err := testboxKeyPath(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := claimLeaseTargetForConfig(id, "stop-lifetime-test", Config{Provider: "aws"}, Server{Provider: "aws"}, SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	claimPath, err := leaseClaimPath(id)
	if err != nil {
		t.Fatal(err)
	}
	return keyPath, claimPath
}
