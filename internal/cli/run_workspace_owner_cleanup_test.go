package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type runCleanupWorkspaceOwnerTransport struct {
	mu sync.Mutex

	blockInspectAt int
	inspectCount   int
	renewErr       error
	destroyed      atomic.Bool

	renewStarted   chan struct{}
	allowRenew     chan struct{}
	renewFinished  chan struct{}
	inspectStarted chan struct{}
	allowInspect   chan struct{}
	ownerReleased  chan struct{}

	renewStartOnce   sync.Once
	allowRenewOnce   sync.Once
	renewFinishOnce  sync.Once
	inspectOnce      sync.Once
	allowInspectOnce sync.Once
	ownerReleaseOnce sync.Once
}

func newRunCleanupWorkspaceOwnerTransport(blockInspectAt int, renewErr error) *runCleanupWorkspaceOwnerTransport {
	return &runCleanupWorkspaceOwnerTransport{
		blockInspectAt: blockInspectAt,
		renewErr:       renewErr,
		renewStarted:   make(chan struct{}),
		allowRenew:     make(chan struct{}),
		renewFinished:  make(chan struct{}),
		inspectStarted: make(chan struct{}),
		allowInspect:   make(chan struct{}),
		ownerReleased:  make(chan struct{}),
	}
}

func (r *runCleanupWorkspaceOwnerTransport) Do(ctx context.Context, req workspaceOwnerRemoteRequest) (string, error) {
	switch req.Action {
	case workspaceOwnerRenew:
		r.renewStartOnce.Do(func() { close(r.renewStarted) })
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-r.allowRenew:
		}
		r.renewFinishOnce.Do(func() { close(r.renewFinished) })
		if r.destroyed.Load() {
			return "", errors.New("renew transport destroyed with lease")
		}
		if r.renewErr != nil {
			return "", r.renewErr
		}
		return "RENEWED", nil
	case workspaceOwnerInspect:
		r.mu.Lock()
		r.inspectCount++
		blocked := r.inspectCount == r.blockInspectAt
		r.mu.Unlock()
		if blocked {
			r.inspectOnce.Do(func() { close(r.inspectStarted) })
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-r.allowInspect:
			}
		}
		return "OWNED", nil
	case workspaceOwnerRelease:
		r.ownerReleaseOnce.Do(func() { close(r.ownerReleased) })
		return "RELEASED", nil
	default:
		return "", errors.New("unexpected workspace-owner action")
	}
}

func (r *runCleanupWorkspaceOwnerTransport) unblockRenewal() {
	r.allowRenewOnce.Do(func() { close(r.allowRenew) })
}

func (r *runCleanupWorkspaceOwnerTransport) unblockInspect() {
	r.allowInspectOnce.Do(func() { close(r.allowInspect) })
}

func (r *runCleanupWorkspaceOwnerTransport) acquire(ctx context.Context, target SSHTarget, leaseID string, _ io.Writer) (*workspaceOwner, error) {
	ownerCtx, cancel := context.WithCancel(ctx)
	owner := &workspaceOwner{
		target:    target,
		transport: r,
		key:       workspaceOwnerKey(leaseID),
		token:     strings.Repeat("a", 64),
		ttl:       time.Minute,
		ctx:       ownerCtx,
		cancel:    cancel,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	ticks := make(chan time.Time, 1)
	go owner.renewLoopWithTicks(ticks, time.Minute)
	ticks <- time.Now()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.renewStarted:
		return owner, nil
	}
}

func waitRunCleanupSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

type runCleanupAsyncResult struct {
	done chan struct{}
	err  error
}

func startRunCleanupAsync(t *testing.T, remote *runCleanupWorkspaceOwnerTransport, run func(context.Context) error) *runCleanupAsyncResult {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	result := &runCleanupAsyncResult{done: make(chan struct{})}
	go func() {
		result.err = run(ctx)
		close(result.done)
	}()
	t.Cleanup(func() {
		remote.unblockInspect()
		remote.unblockRenewal()
		cancel()
		select {
		case <-result.done:
		case <-time.After(5 * time.Second):
			t.Errorf("run goroutine did not stop during cleanup")
		}
	})
	return result
}

func (r *runCleanupAsyncResult) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-r.done:
		return r.err
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish")
		return nil
	}
}

func assertRunCleanupExitCode(t *testing.T, err error, want int, stdout, stderr string) {
	t.Helper()
	if want == 0 {
		if err != nil {
			t.Fatalf("error=%v, want success\nstdout=%s\nstderr=%s", err, stdout, stderr)
		}
		return
	}
	var exitErr ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != want {
		t.Fatalf("error=%v exit=%d, want %d\nstdout=%s\nstderr=%s", err, exitErr.Code, want, stdout, stderr)
	}
}

func setupRunCleanupWorkspaceOwnerTest(t *testing.T) string {
	t.Helper()
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	t.Chdir(dir)
	sshPath := filepath.Join(dir, "ssh")
	commandScript := `#!/bin/sh
case "$1" in
  *"base64 <"*) printf 'cHJvb2YtZG93bmxvYWRlZAo='; exit 0 ;;
  *"renewal-cleanup-exit-23"*) exit 23 ;;
esac
exit 0
`
	installWorkspaceOwnerAwareSSH(t, sshPath, commandScript)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
	runEnvProfileTestAcquireLease = nil
	runEnvProfileTestAcquireHook = nil
	runEnvProfileTestReleaseHook = nil
	runEnvProfileTestReleaseRequestHook = nil
	runEnvProfileTestTouchHook = nil
	runEnvProfileTestReleaseErr = nil
	runEnvProfileTestConnectionCleanupSafe = true
	t.Cleanup(func() {
		runEnvProfileTestAcquireLease = nil
		runEnvProfileTestAcquireHook = nil
		runEnvProfileTestReleaseHook = nil
		runEnvProfileTestReleaseRequestHook = nil
		runEnvProfileTestTouchHook = nil
		runEnvProfileTestReleaseErr = nil
		runEnvProfileTestConnectionCleanupSafe = true
		removeLeaseClaim("cbx_env_profile_test")
	})
	return dir
}

func TestRunCommandDestructiveCleanupQuiescesWorkspaceOwner(t *testing.T) {
	tests := []struct {
		name             string
		command          string
		renewErr         error
		stopErr          error
		download         bool
		wantExit         int
		wantStop         bool
		wantOwnerRelease bool
	}{
		{name: "successful evidence-backed run", command: "renewal-cleanup-success", download: true, wantStop: true},
		{name: "renewal fails before stop", command: "renewal-cleanup-success", renewErr: errors.New("renew response lost"), wantExit: 7, wantOwnerRelease: true},
		{name: "stop is not confirmed", command: "renewal-cleanup-success", stopErr: errors.New("stop not confirmed"), wantExit: 7, wantStop: true, wantOwnerRelease: true},
		{name: "remote command remains nonzero", command: "renewal-cleanup-exit-23", wantExit: 23, wantStop: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := setupRunCleanupWorkspaceOwnerTest(t)
			remote := newRunCleanupWorkspaceOwnerTransport(2, test.renewErr)
			releaseStarted := make(chan struct{})
			var releaseOnce sync.Once
			var releaseOvertookRenewal atomic.Bool
			runEnvProfileTestReleaseHook = func() error {
				remote.destroyed.Store(true)
				releaseOnce.Do(func() { close(releaseStarted) })
				select {
				case <-remote.renewFinished:
				default:
					releaseOvertookRenewal.Store(true)
					remote.unblockRenewal()
				}
				return test.stopErr
			}

			downloadPath := filepath.Join(dir, "proof.txt")
			args := []string{
				"--provider", runEnvProfileTestProvider{}.Name(),
				"--id", "cbx_env_profile_test",
				"--no-sync",
				"--no-hydrate",
				"--stop-after", "always",
			}
			if test.download {
				args = append(args, "--download", "proof.txt="+downloadPath)
			}
			args = append(args, "--", test.command)
			var stdout, stderr bytes.Buffer
			ownerReady := make(chan *workspaceOwner, 1)
			app := App{
				Stdout: &stdout,
				Stderr: &stderr,
				workspaceOwnerAcquirer: func(ctx context.Context, target SSHTarget, leaseID string, stderr io.Writer) (*workspaceOwner, error) {
					owner, err := remote.acquire(ctx, target, leaseID, stderr)
					if err == nil {
						ownerReady <- owner
					}
					return owner, err
				},
			}
			result := startRunCleanupAsync(t, remote, func(ctx context.Context) error {
				return app.runCommand(ctx, args)
			})
			var owner *workspaceOwner
			select {
			case owner = <-ownerReady:
			case <-time.After(5 * time.Second):
				t.Fatal("run did not acquire workspace owner")
			}

			waitRunCleanupSignal(t, remote.inspectStarted, "destructive-cleanup owner inspection")
			if test.download {
				data, err := os.ReadFile(downloadPath)
				if err != nil || string(data) != "proof-downloaded\n" {
					t.Fatalf("download before cleanup=%q err=%v", data, err)
				}
			}
			remote.unblockInspect()
			select {
			case <-releaseStarted:
				t.Fatal("destructive stop overtook workspace-owner renewal quiescence")
			case <-time.After(5 * time.Second):
				t.Fatal("workspace-owner renewal was not asked to quiesce")
			case <-owner.stop:
			}
			select {
			case <-releaseStarted:
				t.Fatal("destructive stop began while renewal transport was still in flight")
			default:
			}
			remote.unblockRenewal()

			runErr := result.wait(t)
			if releaseOvertookRenewal.Load() {
				t.Fatal("backend release observed renewal still in flight")
			}
			assertRunCleanupExitCode(t, runErr, test.wantExit, stdout.String(), stderr.String())
			select {
			case <-releaseStarted:
				if !test.wantStop {
					t.Fatal("destructive stop ran after pre-stop renewal failure")
				}
			default:
				if test.wantStop {
					t.Fatal("destructive stop did not run")
				}
			}
			select {
			case <-remote.ownerReleased:
				if !test.wantOwnerRelease {
					t.Fatal("confirmed stop attempted a remote owner release")
				}
			default:
				if test.wantOwnerRelease {
					t.Fatal("failed or skipped stop did not release the remote owner")
				}
			}
		})
	}
}

func TestRunCommandRetainedLeaseRetainsFailClosedRenewal(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "new lease explicitly kept", args: []string{"--keep"}},
		{name: "reused lease normal lifecycle", args: []string{"--id", "cbx_env_profile_test"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setupRunCleanupWorkspaceOwnerTest(t)
			remote := newRunCleanupWorkspaceOwnerTransport(1, errors.New("renew response lost"))
			releaseStarted := make(chan struct{})
			var releaseOnce sync.Once
			runEnvProfileTestReleaseHook = func() error {
				releaseOnce.Do(func() { close(releaseStarted) })
				return nil
			}
			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr, workspaceOwnerAcquirer: remote.acquire}
			args := []string{
				"--provider", runEnvProfileTestProvider{}.Name(),
				"--no-sync",
				"--no-hydrate",
			}
			args = append(args, test.args...)
			args = append(args, "--", "renewal-cleanup-success")
			result := startRunCleanupAsync(t, remote, func(ctx context.Context) error {
				return app.runCommand(ctx, args)
			})
			waitRunCleanupSignal(t, remote.inspectStarted, "post-command owner inspection")
			remote.unblockInspect()
			remote.unblockRenewal()
			runErr := result.wait(t)
			assertRunCleanupExitCode(t, runErr, 7, stdout.String(), stderr.String())
			select {
			case <-releaseStarted:
				t.Fatal("retained lease was destructively stopped")
			default:
			}
			waitRunCleanupSignal(t, remote.ownerReleased, "retained owner release")
		})
	}
}
