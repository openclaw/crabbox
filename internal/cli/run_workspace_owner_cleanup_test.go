package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
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
	inspectReply              string
	childMarker               string
	inspectErr                error
	renewReply                string
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
			return r.renewReply, r.renewErr
		}
		return firstNonBlank(r.renewReply, "RENEWED"), nil
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
		if r.inspectErr != nil {
			return r.inspectReply, r.inspectErr
		}
		if r.childMarker != "" {
			if _, err := os.Stat(r.childMarker); err == nil {
				return "CHILD", nil
			}
		}
		return firstNonBlank(r.inspectReply, "OWNED"), nil
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
	downloadedProof := []byte("proof-downloaded\n")
	commandScript := `#!/bin/sh
printf '%s\n---\n' "$1" >> "$CRABBOX_FAKE_SSH_LOG"
case "$1" in
  *"base64 <"*) printf '%s' ` + shellQuote(encodedRunDownloadPayload(int64(len(downloadedProof)), downloadedProof)) + `; exit 0 ;;
  *"renewal-cleanup-exit-23"*) exit 23 ;;
esac
exit 0
`
	installWorkspaceOwnerAwareSSH(t, sshPath, commandScript)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_FAKE_SSH_LOG", filepath.Join(dir, "ssh.log"))
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
	runEnvProfileTestRetainsLease = false
	runEnvProfileTestTerminalReleaseError = false
	t.Cleanup(func() {
		runEnvProfileTestAcquireLease = nil
		runEnvProfileTestAcquireHook = nil
		runEnvProfileTestReleaseHook = nil
		runEnvProfileTestReleaseRequestHook = nil
		runEnvProfileTestTouchHook = nil
		runEnvProfileTestReleaseErr = nil
		runEnvProfileTestConnectionCleanupSafe = true
		runEnvProfileTestPreservesSSHWorkspace = false
		runEnvProfileTestRetainsLease = false
		runEnvProfileTestTerminalReleaseError = false
		RemoveLeaseClaim("cbx_env_profile_test")
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
				"--provider", runEnvProfileTestProvider{}.Spec().Name,
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
		name           string
		args           []string
		renewReply     string
		renewErr       error
		wantDiagnostic string
		forbidOutput   string
	}{
		{name: "new lease explicitly kept", args: []string{"--keep"}, renewErr: errors.New("renew response lost")},
		{name: "reused lease normal lifecycle", args: []string{"--id", "cbx_env_profile_test"}, renewErr: errors.New("renew response lost")},
		{name: "mismatch renewal denial", args: []string{"--id", "cbx_env_profile_test", "--stop-after", "always"}, renewReply: "MISMATCH", renewErr: errors.New("exit status 75"), wantDiagnostic: "remote workspace owner renewal failed closed: protocol state MISMATCH: exit status 75"},
		{name: "expired renewal denial", args: []string{"--id", "cbx_env_profile_test", "--stop-after", "always"}, renewReply: "EXPIRED", renewErr: errors.New("exit status 75"), wantDiagnostic: "remote workspace owner renewal failed closed: protocol state EXPIRED: exit status 75"},
		{name: "ambiguous renewal denial", args: []string{"--id", "cbx_env_profile_test", "--stop-after", "always"}, renewReply: "AMBIGUOUS", renewErr: errors.New("exit status 74"), wantDiagnostic: "remote workspace owner renewal failed closed: protocol state AMBIGUOUS: exit status 74"},
		{name: "unknown renewal denial", args: []string{"--id", "cbx_env_profile_test", "--stop-after", "always"}, renewReply: "UNKNOWN raw-renew-output", renewErr: errors.New("exit status 75"), wantDiagnostic: "remote workspace owner renewal failed closed: exit status 75", forbidOutput: "raw-renew-output"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := setupRunCleanupWorkspaceOwnerTest(t)
			remote := newRunCleanupWorkspaceOwnerTransport(1, test.renewErr)
			remote.renewReply = test.renewReply
			if test.renewReply != "" {
				remote.inspectReply, remote.inspectErr = test.renewReply, test.renewErr
			}
			releaseStarted := make(chan struct{})
			var releaseOnce sync.Once
			runEnvProfileTestReleaseHook = func() error {
				releaseOnce.Do(func() { close(releaseStarted) })
				return nil
			}
			var stdout, stderr bytes.Buffer
			ownerReady := make(chan *workspaceOwner, 1)
			app := App{Stdout: &stdout, Stderr: &stderr, workspaceOwnerAcquirer: func(ctx context.Context, target SSHTarget, leaseID string, stderr io.Writer) (*workspaceOwner, error) {
				owner, err := remote.acquire(ctx, target, leaseID, stderr)
				if err == nil {
					ownerReady <- owner
				}
				return owner, err
			}}
			downloadPath := filepath.Join(dir, "proof.txt")
			args := []string{
				"--provider", runEnvProfileTestProvider{}.Spec().Name,
				"--no-sync",
				"--no-hydrate",
				"--junit", "report.xml",
				"--artifact-glob", "artifacts/*.txt",
				"--download", "proof.txt=" + downloadPath,
			}
			args = append(args, test.args...)
			args = append(args, "--", "renewal-cleanup-success")
			result := startRunCleanupAsync(t, remote, func(ctx context.Context) error {
				return app.runCommand(ctx, args)
			})
			var owner *workspaceOwner
			select {
			case owner = <-ownerReady:
			case <-time.After(runCleanupTestTimeout):
				t.Fatal("run did not acquire workspace owner")
			}
			waitRunCleanupSignal(t, remote.inspectStarted, "post-command owner inspection")
			sshBeforeDenial, err := os.ReadFile(filepath.Join(dir, "ssh.log"))
			if err != nil {
				t.Fatal(err)
			}
			remote.unblockRenewal()
			waitRunCleanupSignal(t, owner.ctx.Done(), "failed renewal owner cancellation")
			waitRunCleanupSignal(t, owner.done, "failed renewal loop completion")
			remote.unblockInspect()
			runErr := result.wait(t)
			assertRunCleanupExitCode(t, runErr, 7, stdout.String(), stderr.String())
			if owner.Err() == nil {
				t.Fatal("failed renewal did not retain the owner error")
			}
			if test.wantDiagnostic != "" && !strings.Contains(runErr.Error(), test.wantDiagnostic) {
				t.Errorf("error=%v, want diagnostic %q", runErr, test.wantDiagnostic)
			}
			if test.forbidOutput != "" && strings.Contains(runErr.Error()+stdout.String()+stderr.String(), test.forbidOutput) {
				t.Errorf("untrusted renewal output reached run diagnostics: %v\n%s\n%s", runErr, stdout.String(), stderr.String())
			}
			sshAfterDenial, err := os.ReadFile(filepath.Join(dir, "ssh.log"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(sshBeforeDenial, sshAfterDenial) {
				t.Fatalf("remote commands ran after renewal denied result, artifact, download, and cleanup authority:\nbefore=%s\nafter=%s", sshBeforeDenial, sshAfterDenial)
			}
			if _, err := os.Stat(downloadPath); !os.IsNotExist(err) {
				t.Fatalf("download ran after renewal denial: stat err=%v", err)
			}
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
		name          string
		flags         []string
		stopErr       error
		retained      bool
		terminalError bool
		wantStop      bool
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
		{name: "retained direct release", retained: true},
		{name: "deleted with local cleanup error", stopErr: errors.New("local cleanup failed"), terminalError: true, wantStop: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupRunCleanupWorkspaceOwnerTest(t)
			runEnvProfileTestRetainsLease = test.retained
			runEnvProfileTestTerminalReleaseError = test.terminalError
			var stdout, stderr bytes.Buffer
			var releaseCalls int
			runEnvProfileTestReleaseHook = func() error {
				releaseCalls++
				if strings.Contains(stderr.String(), "failure digest") {
					t.Error("digest printed before cleanup outcome was known")
				}
				return test.stopErr
			}
			args := []string{"--provider", runEnvProfileTestProvider{}.Spec().Name, "--no-sync", "--no-hydrate", "--timing-json"}
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
			if report.RunStatus != "failed" || report.RetryLikely != "unknown" || (report.LeaseStopErr != "") != (test.stopErr != nil) {
				t.Errorf("run status or release error changed: %#v", report)
			}
			if !strings.Contains(out, "failure digest") {
				t.Fatalf("missing failure digest:\n%s", out)
			}
			for _, command := range []struct {
				name string
				want bool
			}{
				{name: "ssh", want: !test.wantStop},
				{name: "run"},
				{name: "stop", want: !test.wantStop},
			} {
				if got := strings.Contains(out, "next: crabbox "+command.name+" "); got != command.want {
					t.Errorf("recovery %s present=%v want=%v, stopped=%v:\n%s", command.name, got, command.want, test.wantStop, out)
				}
			}
			if (test.wantStop || test.stopErr != nil || test.retained) != (releaseCalls == 1) {
				t.Errorf("release calls=%d", releaseCalls)
			}
		})
	}
}

func TestRunCommandFreshWindowsLeaseAcquiresInputOwner(t *testing.T) {
	setupRunCleanupWorkspaceOwnerTest(t)
	const leaseID = "cbx_env_profile_test"
	runEnvProfileTestAcquireLease = func(AcquireRequest) (LeaseTarget, error) {
		return LeaseTarget{
			LeaseID: leaseID,
			Server:  Server{Provider: runEnvProfileTestProvider{}.Spec().Name},
			SSH:     SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeNormal, SSHConfigProxy: true},
		}, nil
	}
	ownerBoundary := errors.New("stop before Windows input can bypass its owner")
	ownerCalls, releases := 0, 0
	runEnvProfileTestReleaseHook = func() error { releases++; return nil }
	var output bytes.Buffer
	app := App{
		Stdout: &output,
		Stderr: &output,
		workspaceOwnerAcquirer: func(_ context.Context, target SSHTarget, id string, _ io.Writer) (*workspaceOwner, error) {
			ownerCalls++
			if id != leaseID || !isWindowsNativeTarget(target) {
				t.Fatalf("wrong input owner target: id=%s target=%+v", id, target)
			}
			return nil, ownerBoundary
		},
	}
	err := app.runCommand(t.Context(), []string{"--provider", runEnvProfileTestProvider{}.Spec().Name, "--target", targetWindows, "--windows-mode", windowsModeNormal, "--no-sync", "--no-hydrate", "--", "Write-Output", "must-not-run"})
	if !errors.Is(err, ownerBoundary) || ownerCalls != 1 || releases != 1 {
		t.Fatalf("fresh Windows input bypassed owner or cleanup: err=%v owners=%d releases=%d\n%s", err, ownerCalls, releases, output.String())
	}
}

func TestReleaseReplacementLeaseTransfersOwnerLifecycle(t *testing.T) {
	for _, test := range []struct {
		name      string
		preserves bool
		terminal  bool
	}{
		{name: "destroyed", terminal: true},
		{name: "preserved", preserves: true},
		{name: "terminal preserved", preserves: true, terminal: true},
	} {
		preserves := test.preserves
		t.Run(test.name, func(t *testing.T) {
			setupRunCleanupWorkspaceOwnerTest(t)
			runEnvProfileTestPreservesSSHWorkspace = preserves
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			var owner *workspaceOwner
			resolved := true
			var events []string
			for _, id := range []string{"lease-a", "lease-b"} {
				if workspaceOwnerFromContext(parent) != nil || parent.Err() != nil {
					t.Fatal("replacement acquisition inherited the previous owner or cancellation")
				}
				remote := newRunCleanupWorkspaceOwnerTransport(0, nil)
				remote.unblockRenewal()
				var err error
				owner, err = remote.acquire(parent, SSHTarget{}, id, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, "acquire:"+id)
				resolved = true
				previous := owner
				err = releaseReplacementLease(parent, &owner, &resolved, runEnvProfileTestBackend{}, func(ctx context.Context) (ReleaseLeaseOutcome, error) {
					select {
					case <-previous.done:
					default:
						t.Fatal("backend release preceded owner quiescence")
					}
					remote.mu.Lock()
					inspections := remote.inspectCount
					remote.mu.Unlock()
					if inspections != 1 {
						t.Fatalf("owner inspections=%d, want 1 before release", inspections)
					}
					events = append(events, "release:"+id)
					remote.backendReturned.Store(true)
					return ReleaseLeaseOutcome{Terminal: test.terminal}, nil
				})
				if err != nil || owner != nil || resolved {
					t.Fatalf("release: err=%v owner=%p resolved=%t", err, owner, resolved)
				}
				select {
				case <-remote.ownerReleased:
					if !preserves || remote.ownerReleaseBeforeBackend.Load() {
						t.Fatal("owner release occurred against destroyed or unreleased workspace")
					}
				default:
					if preserves {
						t.Fatal("preserved workspace owner was not closed")
					}
				}
				previous.cancel()
			}
			if got := strings.Join(events, ","); got != "acquire:lease-a,release:lease-a,acquire:lease-b,release:lease-b" {
				t.Fatalf("lifecycle order: %s", got)
			}
			cancel()
			if parent.Err() != context.Canceled {
				t.Fatal("replacement lost parent cancellation")
			}
		})
	}
}

func TestReleaseReplacementLeaseReleaseFailure(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "unconfirmed"
		if terminal {
			name = "terminal"
		}
		t.Run(name, func(t *testing.T) {
			setupRunCleanupWorkspaceOwnerTest(t)
			runEnvProfileTestPreservesSSHWorkspace = terminal
			remote := newRunCleanupWorkspaceOwnerTransport(0, nil)
			remote.unblockRenewal()
			owner, err := remote.acquire(t.Context(), SSHTarget{}, "lease-a", io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.cancel()
			previous := owner
			resolved := true
			releaseErr := errors.New("release bookkeeping failed")
			err = releaseReplacementLease(t.Context(), &owner, &resolved, runEnvProfileTestBackend{}, func(context.Context) (ReleaseLeaseOutcome, error) {
				return ReleaseLeaseOutcome{Terminal: terminal}, releaseErr
			})
			if !errors.Is(err, releaseErr) {
				t.Fatalf("release error lost: %v", err)
			}
			if terminal {
				if owner != previous || resolved {
					t.Fatal("terminal preserved lease lost diagnostic owner or retained automatic cleanup state")
				}
			} else if owner != previous || !resolved {
				t.Fatal("unconfirmed release lost diagnostics or cleanup state")
			}
			select {
			case <-remote.ownerReleased:
				t.Fatal("failed release attempted guest owner writes")
			default:
			}
		})
	}
}

func TestReleaseReplacementLeaseRequiresConfirmedOutcome(t *testing.T) {
	setupRunCleanupWorkspaceOwnerTest(t)
	runEnvProfileTestPreservesSSHWorkspace = false
	remote := newRunCleanupWorkspaceOwnerTransport(0, nil)
	remote.unblockRenewal()
	owner, err := remote.acquire(t.Context(), SSHTarget{}, "lease-a", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.cancel()
	previous := owner
	resolved := true
	err = releaseReplacementLease(t.Context(), &owner, &resolved, runEnvProfileTestBackend{}, func(context.Context) (ReleaseLeaseOutcome, error) { return ReleaseLeaseOutcome{}, nil })
	if err == nil || owner != previous || !resolved {
		t.Fatalf("unconfirmed outcome: err=%v owner=%p resolved=%t", err, owner, resolved)
	}
	select {
	case <-remote.ownerReleased:
		t.Fatal("unconfirmed outcome attempted guest cleanup")
	default:
	}
}

// Real run and coordinator adapter; simulated provider HTTP, SSH commands, and
// rsync. The listener only admits readiness TCP probes, not SSH handshakes.
type replacementRunProvider struct {
	runEnvProfileTestProvider
	coord *CoordinatorClient
}

func (p replacementRunProvider) Spec() ProviderSpec {
	spec := p.runEnvProfileTestProvider.Spec()
	spec.Name = "run-replacement-test"
	return spec
}
func (p replacementRunProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return &coordinatorLeaseBackend{spec: p.Spec(), cfg: cfg, rt: rt, coord: p.coord, direct: runEnvProfileTestBackend{spec: p.Spec()}}, nil
}

func TestRunCommandReplacesLeaseWithFreshWorkspaceOwner(t *testing.T) {
	for _, scenario := range []string{"success", "command failure", "caller cancellation", "retained release"} {
		t.Run(scenario, func(t *testing.T) {
			dir := setupRunCleanupWorkspaceOwnerTest(t)
			source := filepath.Join(dir, "source")
			if err := os.Mkdir(source, 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(source, "input.txt"), "replacement fixture\n")
			t.Chdir(source)
			configPath := filepath.Join(source, "crabbox.yaml")
			writeFile(t, configPath, "sync:\n  source: directory\n  include: [input.txt]\n  fingerprint: false\n")
			t.Setenv("CRABBOX_CONFIG", configPath)
			syncLog := filepath.Join(dir, "sync.log")
			t.Setenv("CRABBOX_REPLACEMENT_SYNC_LOG", syncLog)
			if err := os.WriteFile(filepath.Join(dir, "rsync"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CRABBOX_REPLACEMENT_SYNC_LOG\"\n/bin/cat >/dev/null\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			installWorkspaceOwnerAwareSSH(t, filepath.Join(dir, "ssh"), `#!/bin/sh
printf '%s\n---\n' "$1" >> "$CRABBOX_FAKE_SSH_LOG"
case "$1" in
  *replacement-workload-fail*) printf 'replacement workload completed\n'; exit 23 ;;
  *replacement-workload*) printf 'replacement workload completed\n' ;;
esac
/bin/cat >/dev/null
exit 0
`)
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			callerKey := new(int)
			parent, cancel := context.WithCancel(context.WithValue(t.Context(), callerKey, "caller"))
			defer cancel()
			var mu sync.Mutex
			var events []string
			addEvent := func(event string) { mu.Lock(); events = append(events, event); mu.Unlock() }
			owners := map[string]*workspaceOwner{}
			remotes := map[string]*runCleanupWorkspaceOwnerTransport{}
			leases := map[string]CoordinatorLease{}
			var ids []string
			var receipt terminalRunReceipt
			var stdout, stderr bytes.Buffer
			provider := replacementRunProvider{}
			coord := &CoordinatorClient{BaseURL: "http://replacement.invalid", Token: "fixture"}
			coord.Client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				var response any
				code := http.StatusOK
				parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
					if workspaceOwnerFromContext(r.Context()) != nil || r.Context().Err() != nil || parent.Err() != nil || r.Context().Value(callerKey) != "caller" {
						t.Error("allocation inherited stale owner/cancellation")
					}
					var body struct {
						LeaseID string `json:"leaseID"`
						Slug    string `json:"slug"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						return nil, err
					}
					ids = append(ids, body.LeaseID)
					events = append(events, "acquire:"+body.LeaseID)
					lease := CoordinatorLease{ID: body.LeaseID, Slug: body.Slug, Provider: provider.Spec().Name, TargetOS: targetLinux, Host: "127.0.0.1", SSHUser: "crabbox", SSHPort: port, SSHHostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICFNHmH+uXzuQadD4Pg9JhPQvl5fkM4L9spUDQ/mI+pc", WorkRoot: "/work", State: "active"}
					leases[lease.ID] = lease
					response = map[string]any{"lease": lease}
				case len(parts) >= 3 && parts[1] == "leases":
					id := parts[2]
					lease := leases[id]
					if len(parts) == 4 && parts[3] == "release" {
						owner := owners[id]
						if owner == nil {
							t.Error("release lacks workspace owner")
						} else {
							select {
							case <-owner.done:
							default:
								t.Error("release precedes quiescence")
							}
						}
						events = append(events, "release:"+id)
						if scenario == "retained release" {
							retained := false
							lease.State, lease.CleanupStatus, lease.ReleaseDeletesServer = "released", "retained", &retained
							leases[id] = lease
							remotes[id].backendReturned.Store(true)
							response = map[string]any{"lease": lease}
							break
						}
						remotes[id].backendReturned.Store(true)
						owner.cancel()
						lease.State, lease.CleanupStatus, lease.CleanupCompletedAt = "released", "complete", "2026-09-16T00:00:00Z"
						lease.Host, lease.SSHHostKey = "", ""
						leases[id] = lease
						if scenario == "caller cancellation" {
							cancel()
						}
					}
					response = map[string]any{"lease": lease}
				case len(parts) >= 3 && parts[1] == "runs":
					if len(parts) == 4 && parts[3] == "receipt" {
						response = map[string]any{"receipt": receipt}
					} else if len(parts) == 4 && parts[3] == "finish" {
						var body struct {
							Receipt terminalRunReceipt `json:"receipt"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							return nil, err
						}
						receipt = body.Receipt
						response = map[string]any{"run": CoordinatorRun{ID: parts[2], State: "succeeded"}}
					} else if len(parts) == 4 && parts[3] == "events" {
						response = map[string]any{"event": CoordinatorRunEvent{RunID: parts[2], Seq: 1}}
					} else {
						var body struct {
							Command []string `json:"command"`
						}
						if r.Method == http.MethodPut {
							if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
								return nil, err
							}
						}
						response = map[string]any{"run": CoordinatorRun{ID: parts[2], Provider: provider.Spec().Name, State: "running", Phase: "starting", Command: body.Command}}
					}
				default:
					code = http.StatusNotFound
					response = map[string]any{"error": "not found"}
				}
				data, err := json.Marshal(response)
				return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, err
			})}
			provider.coord = coord
			RegisterProvider(provider)
			t.Cleanup(func() { delete(providerRegistry, provider.Spec().Name) })
			app := App{Stdout: &stdout, Stderr: &stderr}
			app.workspaceOwnerAcquirer = func(ctx context.Context, target SSHTarget, id string, out io.Writer) (*workspaceOwner, error) {
				if workspaceOwnerFromContext(ctx) != nil || ctx.Err() != nil {
					t.Fatal("owner acquisition inherited old owner")
				}
				remote := newRunCleanupWorkspaceOwnerTransport(0, nil)
				remote.childMarker = filepath.Join(dir, "owner-child")
				remote.unblockRenewal()
				owner, err := remote.acquire(ctx, target, id, out)
				mu.Lock()
				owners[id], remotes[id] = owner, remote
				mu.Unlock()
				addEvent("owner:" + id)
				return owner, err
			}
			app.sshReadinessWaiter = func(ctx context.Context, target *SSHTarget, out io.Writer, phase string, timeout time.Duration) error {
				if phase == "before sync" || phase == "before command" {
					owner := workspaceOwnerFromContext(ctx)
					mu.Lock()
					id := ids[len(ids)-1]
					expected := owners[id]
					first := len(ids) == 1
					mu.Unlock()
					if owner == nil || owner != expected {
						t.Fatalf("%s lacks current owner", phase)
					}
					addEvent(phase + ":" + id)
					if phase == "before command" && first {
						return Exit(5, "timed out waiting for SSH during before command")
					}
				}
				return waitForSSHReady(ctx, target, out, phase, timeout)
			}
			command := "replacement-workload"
			if scenario == "command failure" {
				command += "-fail"
			}
			// Coordinator acquisitions are exclusive: failure retention requests an owner.
			// Success closes retained B's owner; command failure destroys B.
			err = app.runCommand(parent, []string{"--provider", provider.Spec().Name, "--no-hydrate", "--stop-after", "failure", "--", command})
			for _, owner := range owners {
				owner.stopRenewal()
				owner.cancel()
			}
			if scenario == "caller cancellation" || scenario == "retained release" {
				if err == nil || len(ids) != 1 || strings.Contains(stdout.String(), "replacement workload completed") {
					t.Fatalf("unsafe continuation: err=%v ids=%v\n%s", err, ids, stderr.String())
				}
				if !strings.Contains(strings.Join(events, ","), "release:"+ids[0]) {
					t.Fatalf("did not reach A release: %v", events)
				}
				if scenario == "caller cancellation" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation error lost: %v", err)
				}
				if scenario == "retained release" && !strings.Contains(err.Error(), "did not confirm a terminal or preserved workspace") {
					t.Fatalf("release error lost: %v", err)
				}
				return
			}
			wantExit := 0
			if scenario == "command failure" {
				wantExit = 23
			}
			assertRunCleanupExitCode(t, err, wantExit, stdout.String(), stderr.String())
			if len(ids) != 2 {
				t.Fatalf("acquisitions=%v events=%v", ids, events)
			}
			a, b := ids[0], ids[1]
			if receipt.LeaseID != b {
				t.Fatalf("workload receipt bound to %s, want B %s", receipt.LeaseID, b)
			}
			want := "acquire:" + a + ",owner:" + a + ",before sync:" + a + ",before command:" + a + ",release:" + a + ",acquire:" + b + ",owner:" + b + ",before sync:" + b + ",before command:" + b
			if scenario == "command failure" {
				want += ",release:" + b
			}
			if got := strings.Join(events, ","); got != want {
				t.Fatalf("events=%s want=%s", got, want)
			}
			if owners[a] == owners[b] {
				t.Fatal("replacement reused owner")
			}
			if !strings.Contains(stdout.String(), "replacement workload completed") {
				t.Fatalf("workload missing: %s", stdout.String())
			}
			data, readErr := os.ReadFile(syncLog)
			if readErr != nil || !strings.Contains(string(data), a) || !strings.Contains(string(data), b) {
				t.Fatalf("sync targets=%s err=%v", data, readErr)
			}
			select {
			case <-remotes[a].ownerReleased:
				t.Fatal("destroyed A received owner release")
			default:
			}
			select {
			case <-remotes[b].ownerReleased:
				if wantExit != 0 {
					t.Fatal("destroyed B received owner release")
				}
			default:
				if wantExit == 0 {
					t.Fatal("retained B owner was not closed")
				}
			}
		})
	}
}
