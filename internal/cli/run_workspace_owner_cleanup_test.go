package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

const runCleanupTestTimeout = 30 * time.Second

type runCleanupWorkspaceOwnerTransport struct {
	mu sync.Mutex

	blockInspectAt            int
	inspectCount              int
	renewErr                  error
	destroyed                 atomic.Bool
	releaseReply              string
	releaseErr                error
	backendReturned           atomic.Bool
	ownerReleaseBeforeBackend atomic.Bool
	releaseContextErr         error
	releaseBudget             time.Duration
	releaseEarly              atomic.Bool

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
		defer r.renewFinishOnce.Do(func() { close(r.renewFinished) })
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-r.allowRenew:
		}
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
		r.mu.Lock()
		if deadline, ok := ctx.Deadline(); ok {
			r.releaseBudget = time.Until(deadline)
		}
		r.mu.Unlock()
		select {
		case <-r.renewFinished:
		default:
			r.releaseEarly.Store(true)
		}
		r.releaseContextErr = ctx.Err()
		if !r.backendReturned.Load() {
			r.ownerReleaseBeforeBackend.Store(true)
		}
		r.ownerReleaseOnce.Do(func() { close(r.ownerReleased) })
		if r.releaseErr != nil {
			return "", r.releaseErr
		}
		return firstNonBlank(r.releaseReply, "RELEASED"), ctx.Err()
	default:
		return "", errors.New("unexpected workspace-owner action")
	}
}

func (r *runCleanupWorkspaceOwnerTransport) observedReleaseBudget() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.releaseBudget
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
	case <-time.After(runCleanupTestTimeout):
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
		// Shared hooks and user directories must outlive every run defer.
		<-result.done
	})
	return result
}

func (r *runCleanupAsyncResult) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-r.done:
		return r.err
	case <-time.After(runCleanupTestTimeout):
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
	runEnvProfileTestPreservesSSHWorkspace = false
	t.Cleanup(func() {
		runEnvProfileTestAcquireLease = nil
		runEnvProfileTestAcquireHook = nil
		runEnvProfileTestReleaseHook = nil
		runEnvProfileTestReleaseRequestHook = nil
		runEnvProfileTestTouchHook = nil
		runEnvProfileTestReleaseErr = nil
		runEnvProfileTestConnectionCleanupSafe = true
		runEnvProfileTestPreservesSSHWorkspace = false
		removeLeaseClaim("cbx_env_profile_test")
	})
	return dir
}

func TestRunCommandLeaseCleanupQuiescesWorkspaceOwner(t *testing.T) {
	tests := []struct {
		name                  string
		command               string
		renewErr              error
		stopErr               error
		download              bool
		wantExit              int
		wantStop              bool
		wantOwnerRelease      bool
		preservesSSHWorkspace bool
		releaseReply          string
		releaseErr            error
		cancelParent          bool
	}{
		{name: "successful evidence-backed run", command: "renewal-cleanup-success", download: true, wantStop: true},
		{name: "renewal fails before stop", command: "renewal-cleanup-success", renewErr: errors.New("renew response lost"), wantExit: 7, wantOwnerRelease: true},
		{name: "stop is not confirmed", command: "renewal-cleanup-success", stopErr: errors.New("stop not confirmed"), wantExit: 7, wantStop: true, wantOwnerRelease: true},
		{name: "nonzero with ambiguous owner", command: "renewal-cleanup-exit-23", renewErr: errors.New("renew response lost"), wantExit: 23, wantOwnerRelease: true},
		{name: "nonzero with unconfirmed stop", command: "renewal-cleanup-exit-23", stopErr: errors.New("stop not confirmed"), wantExit: 23, wantStop: true, wantOwnerRelease: true},
		{name: "remote command remains nonzero", command: "renewal-cleanup-exit-23", wantExit: 23, wantStop: true},
		{name: "retained workspace", command: "renewal-cleanup-success", preservesSSHWorkspace: true, wantStop: true, wantOwnerRelease: true},
		{name: "retained workspace command remains nonzero", command: "renewal-cleanup-exit-23", preservesSSHWorkspace: true, wantExit: 23, wantStop: true, wantOwnerRelease: true},
		{name: "retained workspace renewal fails", command: "renewal-cleanup-success", preservesSSHWorkspace: true, renewErr: errors.New("renew response lost"), wantExit: 7, wantOwnerRelease: true},
		{name: "retained workspace backend release fails", command: "renewal-cleanup-success", preservesSSHWorkspace: true, stopErr: errors.New("release not confirmed"), wantExit: 7, wantStop: true, wantOwnerRelease: true},
		{name: "retained workspace close response lost", command: "renewal-cleanup-success", preservesSSHWorkspace: true, releaseErr: errors.New("release response lost"), wantExit: 7, wantStop: true, wantOwnerRelease: true},
		{name: "retained workspace close failure preserves command exit", command: "renewal-cleanup-exit-23", preservesSSHWorkspace: true, releaseErr: errors.New("release response lost"), wantExit: 23, wantStop: true, wantOwnerRelease: true},
		{name: "retained workspace successor owner", command: "renewal-cleanup-success", preservesSSHWorkspace: true, releaseReply: "MISMATCH", wantExit: 7, wantStop: true, wantOwnerRelease: true},
		{name: "retained workspace active child", command: "renewal-cleanup-success", preservesSSHWorkspace: true, releaseReply: "CHILD", wantExit: 7, wantStop: true, wantOwnerRelease: true},
		{name: "retained workspace ambiguous child", command: "renewal-cleanup-success", preservesSSHWorkspace: true, releaseReply: "AMBIGUOUS", wantExit: 7, wantStop: true, wantOwnerRelease: true},
		{name: "retained workspace canceled parent", command: "renewal-cleanup-success", preservesSSHWorkspace: true, cancelParent: true, wantStop: true, wantOwnerRelease: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := setupRunCleanupWorkspaceOwnerTest(t)
			remote := newRunCleanupWorkspaceOwnerTransport(2, test.renewErr)
			remote.releaseReply, remote.releaseErr = test.releaseReply, test.releaseErr
			runEnvProfileTestPreservesSSHWorkspace = test.preservesSSHWorkspace
			var cancelParent context.CancelFunc
			releaseStarted := make(chan struct{})
			var releaseOnce sync.Once
			var releaseOvertookRenewal atomic.Bool
			runEnvProfileTestReleaseHook = func() error {
				defer remote.backendReturned.Store(true)
				remote.destroyed.Store(!test.preservesSSHWorkspace)
				releaseOnce.Do(func() { close(releaseStarted) })
				select {
				case <-remote.renewFinished:
				default:
					releaseOvertookRenewal.Store(true)
					remote.unblockRenewal()
				}
				if test.cancelParent {
					cancelParent()
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
				ctx, cancelParent = context.WithCancel(ctx)
				defer cancelParent()
				return app.runCommand(ctx, args)
			})
			var owner *workspaceOwner
			select {
			case owner = <-ownerReady:
			case <-time.After(runCleanupTestTimeout):
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
			case <-time.After(runCleanupTestTimeout):
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
			if test.command == "renewal-cleanup-exit-23" {
				wantRecovery := test.stopErr != nil || test.renewErr != nil
				if got := strings.Contains(stderr.String(), "next: crabbox ssh "); got != wantRecovery {
					t.Fatalf("lease recovery present=%v want=%v:\n%s", got, wantRecovery, stderr.String())
				}
			}
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
				if test.wantStop && remote.ownerReleaseBeforeBackend.Load() {
					t.Fatal("remote owner released before backend release returned")
				}
				if remote.releaseContextErr != nil {
					t.Fatalf("remote owner release inherited canceled context: %v", remote.releaseContextErr)
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
			if remote.releaseEarly.Load() {
				t.Fatal("owner release began before failed renewal transport cleanup completed")
			}
			if budget := remote.observedReleaseBudget(); budget <= workspaceOwnerCallTimeout-time.Second || budget > workspaceOwnerCallTimeout {
				t.Fatalf("release budget=%s want fresh %s transport call after renewal cleanup", budget, workspaceOwnerCallTimeout)
			}
		})
	}
}

func TestRunFailureDigestCleanupOutcomes(t *testing.T) {
	for _, test := range []struct {
		name     string
		flags    []string
		stopErr  error
		wantStop bool
	}{
		{name: "automatic failure cleanup", wantStop: true},
		{name: "always", flags: []string{"--stop-after", "always"}, wantStop: true},
		{name: "failure", flags: []string{"--stop-after", "failure"}, wantStop: true},
		{name: "success only", flags: []string{"--stop-after", "success"}},
		{name: "never", flags: []string{"--stop-after", "never"}},
		{name: "kept", flags: []string{"--keep"}},
		{name: "keep failed", flags: []string{"--keep-on-failure"}},
		{name: "failed cleanup", stopErr: errors.New("release failed")},
		{name: "ambiguous cleanup", stopErr: errors.New("release response lost")},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupRunCleanupWorkspaceOwnerTest(t)
			var stdout, stderr bytes.Buffer
			var releaseCalls int
			runEnvProfileTestReleaseHook = func() error {
				releaseCalls++
				if strings.Contains(stderr.String(), "failure digest") {
					t.Error("digest printed before cleanup outcome was known")
				}
				return test.stopErr
			}
			args := []string{"--provider", runEnvProfileTestProvider{}.Name(), "--no-sync", "--no-hydrate", "--timing-json"}
			args = append(args, test.flags...)
			args = append(args, "--", "renewal-cleanup-exit-23")
			err := (App{Stdout: &stdout, Stderr: &stderr}).runCommand(context.Background(), args)
			assertRunCleanupExitCode(t, err, 23, stdout.String(), stderr.String())
			out := stderr.String()
			lines := strings.Split(strings.TrimSpace(out), "\n")
			var report TimingReport
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &report); err != nil {
				t.Fatalf("final stderr line must remain timing JSON: %v\n%s", err, out)
			}
			if report.ExitCode != 23 || (report.LeaseStopped != nil && *report.LeaseStopped) != test.wantStop {
				t.Fatalf("timing outcome disagrees with cleanup: %#v", report)
			}
			if !strings.Contains(out, "failure digest") {
				t.Fatalf("missing failure digest:\n%s", out)
			}
			for _, command := range []string{"ssh", "run", "stop"} {
				if got := strings.Contains(out, "next: crabbox "+command+" "); got == test.wantStop {
					t.Errorf("recovery %s present=%v, stopped=%v:\n%s", command, got, test.wantStop, out)
				}
			}
			if (test.wantStop || test.stopErr != nil) != (releaseCalls == 1) {
				t.Errorf("release calls=%d", releaseCalls)
			}
		})
	}
}
