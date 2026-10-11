package azuresandbox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
)

func TestStopInterruptsActiveSandboxOperations(t *testing.T) {
	for _, operation := range []string{"run", "copy", "heartbeat"} {
		t.Run(operation, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			if _, err := b.acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			started := make(chan struct{})
			b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/executeShellCommand") || strings.HasSuffix(r.URL.Path, "/files") {
					close(started)
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				mu.Lock()
				defer mu.Unlock()
				return f.request(t, r)
			})
			file := filepath.Join(t.TempDir(), "input")
			if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				var err error
				switch operation {
				case "run":
					_, err = b.Run(ctx, core.RunRequest{ID: req.RequestedLeaseID, Repo: req.Repo, Keep: true, NoSync: true, Command: []string{"sleep", "3600"}})
				case "copy":
					err = b.Copy(ctx, core.CopyRequest{ID: req.RequestedLeaseID, RepoRoot: req.Repo.Root, Source: file, Destination: "SANDBOX:/tmp/input"})
				case "heartbeat":
					_, err = b.Heartbeat(ctx, core.LeaseHeartbeatRequest{ID: req.RequestedLeaseID})
				}
				done <- err
			}()
			defer func() { cancel(); <-finished }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not start")
			}
			stopCtx, stopCancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer stopCancel()
			stopResults := make(chan error, 2)
			for range 2 {
				go func() { stopResults <- b.Stop(stopCtx, core.StopRequest{ID: req.RequestedLeaseID}) }()
			}
			first, second := <-stopResults, <-stopResults
			if errors.Is(first, context.DeadlineExceeded) || errors.Is(second, context.DeadlineExceeded) || (first != nil && second != nil) {
				t.Fatalf("concurrent stops did not complete: %v / %v", first, second)
			}
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("activity error=%v", err)
			}
			claim, err := b.claim(req.RequestedLeaseID)
			if err != nil || claim.FixedCreateIntent.State != "released" || f.deletes != 1 {
				t.Fatalf("stop did not finish exact deletion: %+v deletes=%d err=%v", claim, f.deletes, err)
			}
		})
	}
}

type blockedActivityOutput struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedActivityOutput) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func TestStopDoesNotWaitForCompletedCommandOutput(t *testing.T) {
	f := &fixture{}
	b, req := fixtureBackend(t, f)
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/executeShellCommand") {
			return response(200, `{"exitCode":0,"stdout":"completed"}`), nil
		}
		return f.request(t, r)
	})
	output := &blockedActivityOutput{started: make(chan struct{}), release: make(chan struct{})}
	b.rt.Stdout = output
	done := make(chan error, 1)
	go func() {
		_, err := b.Run(t.Context(), core.RunRequest{ID: req.RequestedLeaseID, Repo: req.Repo, Keep: true, NoSync: true, Command: []string{"true"}})
		done <- err
	}()
	defer func() { close(output.release); <-done }()
	select {
	case <-output.started:
	case <-time.After(5 * time.Second):
		t.Fatal("command did not reach output rendering")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := b.Stop(ctx, core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatalf("stop waited for local output: %v", err)
	}
	if f.deletes != 1 {
		t.Fatal("stop did not delete completed command's resource")
	}
}

func TestStopSignalRejectsNewActivityAndDoesNotCrossClaimSnapshots(t *testing.T) {
	f := &fixture{}
	b, req := fixtureBackend(t, f)
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	claim, err := b.claim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	path, err := activityStopPath(claim)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := shared.LockOperationFile(t.Context(), path, "test stop signal")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	called := false
	err = withActivity(t.Context(), claim, func(context.Context) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("new activity bypassed stop: err=%v called=%t", err, called)
	}
	before, err := b.claim(req.RequestedLeaseID)
	if err != nil || !reflect.DeepEqual(claim, before) {
		t.Fatal("signal modified claim")
	}
	// Exercise a real reclaim while a stale signal is still held. It must not
	// cancel execution admitted by the new repository owner's claim snapshot.
	req.Repo.Root = t.TempDir()
	if err := core.ClaimLeaseForRepoProviderScopePond(claim.LeaseID, claim.Slug, providerName, claim.ProviderScope, claim.Pond, req.Repo.Root, b.cfg.IdleTimeout, true); err != nil {
		t.Fatal(err)
	}
	b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/executeShellCommand") {
			return response(200, `{"exitCode":0}`), nil
		}
		return f.request(t, r)
	})
	if _, err := b.Run(t.Context(), core.RunRequest{ID: req.RequestedLeaseID, Repo: req.Repo, Keep: true, NoSync: true, Command: []string{"true"}}); err != nil {
		t.Fatalf("stale stop interrupted new owner: %v", err)
	}
	claim, err = b.claim(req.RequestedLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := withActivity(t.Context(), claim, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("released signal blocked later activity: %v", err)
	}
}

func TestSandboxActivityProcessHelper(t *testing.T) {
	if os.Getenv("CRABBOX_TEST_ACA_ACTIVITY_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	claim, err := core.ReadLeaseClaim("cbx_abcdef123456")
	if err != nil {
		t.Fatal(err)
	}
	err = withActivity(t.Context(), claim, func(ctx context.Context) error {
		if err := os.WriteFile(os.Getenv("CRABBOX_TEST_ACA_ACTIVITY_READY"), []byte("ready"), 0600); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stop did not cancel subprocess: %v", err)
	}
}

func TestStopInterruptsActivityInAnotherProcess(t *testing.T) {
	f := &fixture{}
	b, req := fixtureBackend(t, f)
	if _, err := b.acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	log, err := os.Create(filepath.Join(dir, "child.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSandboxActivityProcessHelper$", "-test.timeout=15s")
	cmd.Env = append(os.Environ(), "CRABBOX_TEST_ACA_ACTIVITY_CHILD=1", "CRABBOX_TEST_ACA_ACTIVITY_READY="+ready)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not acquire its activity fence")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := b.Stop(ctx, core.StopRequest{ID: req.RequestedLeaseID}); err != nil {
		t.Fatalf("stop blocked behind child: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 1 {
		t.Fatalf("delete count=%d", f.deletes)
	}
}

func TestStopSignalDoesNotBypassOwnershipOrAbsenceReconciliation(t *testing.T) {
	for _, scenario := range []string{"foreign-labels", "foreign-repository", "already-absent"} {
		t.Run(scenario, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			b.rt.Stdout, b.rt.Stderr = io.Discard, io.Discard
			if _, err := b.acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if scenario == "foreign-labels" {
				f.box.Labels["crabbox_attempt"] = "foreign"
			}
			if scenario == "already-absent" {
				f.box = nil
			}
			root := req.Repo.Root
			if scenario == "foreign-repository" {
				root = t.TempDir()
			}
			err := b.StopForRepository(t.Context(), core.StopRequest{ID: req.RequestedLeaseID}, root)
			if (err == nil) != (scenario == "already-absent") || f.deletes != 0 {
				t.Fatalf("stop error=%v deletes=%d", err, f.deletes)
			}
		})
	}
}
