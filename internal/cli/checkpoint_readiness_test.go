package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func checkpointWaitContext(ctx context.Context, timeout time.Duration) context.Context {
	return context.WithValue(ctx, checkpointReadinessKey{}, checkpointReadinessWait{Timeout: timeout, LeaseID: "cbx_012345abcdef", CheckpointID: "chk_test"})
}

func TestCheckpointReadinessControlAndSlowSuccess(t *testing.T) {
	for _, phase := range []string{"bootstrap", "machine0", "local container ssh"} {
		t.Run(phase, func(t *testing.T) {
			for _, treatment := range []bool{false, true} {
				t.Run(map[bool]string{false: "control", true: "continued wait"}[treatment], func(t *testing.T) {
					target, dir := readinessWaitFixture(t, "ready")
					target.ReadinessStatus = "printf 'pending ownership-repair'"
					// Use the actual guest-loop fixture; the guest polls outlast the control deadline.
					if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nfor remote; do :; done\nexec sh -c \"$remote\"\n"), 0755); err != nil {
						t.Fatal(err)
					}
					ctx := t.Context()
					if treatment {
						ctx = checkpointWaitContext(ctx, 5*time.Second)
					}
					var out bytes.Buffer
					err := waitForSSHReady(ctx, &target, &out, phase, 100*time.Millisecond)
					if treatment && err != nil {
						t.Fatal(err)
					}
					if !treatment && (err == nil || !strings.Contains(err.Error(), "timed out waiting for SSH")) {
						t.Fatalf("control: %v", err)
					}
					if treatment && !strings.Contains(out.String(), "phase=ownership-repair") {
						t.Fatalf("missing phase: %s", out.String())
					}
					t.Logf("treatment=%t error=%v progress=%s", treatment, err, out.String())
				})
			}
		})
	}
}

func TestCheckpointReadinessOutcomesAndResume(t *testing.T) {
	for _, tc := range []struct{ name, mode, status, want string }{
		{"pending", "ready", "printf 'pending ownership-repair'", "state=pending"},
		{"terminal", "ready", "printf 'failed bootstrap'", "state=bootstrap_failed"},
		{"transport", "transport-failure", "printf 'pending bootstrap'", "state=transport_failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, _ := readinessWaitFixture(t, tc.mode)
			// Fixture handles status separately from ready/wait/transport commands.
			target.ReadinessStatus = tc.status
			dir := filepath.Dir(os.Getenv("CRABBOX_WAIT_CALLS"))
			script := "#!/bin/sh\nfor remote; do :; done\n"
			if tc.mode == "transport-failure" {
				script += "exit 255\n"
			} else {
				script += "case \"$remote\" in printf*) exec sh -c \"$remote\";; 'exit 0') exit 0;; esac\nexit 1\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			ctx := checkpointWaitContext(t.Context(), 5*time.Second)
			probeCtx, stop := context.WithCancelCause(ctx)
			defer stop(nil)
			progress := writerFunc(func(p []byte) (int, error) {
				if tc.name == "pending" && strings.Contains(string(p), "ports="+target.Port+":ready") || tc.name == "transport" && strings.Contains(string(p), "ssh-transport") {
					stop(context.DeadlineExceeded)
				}
				return len(p), nil
			})
			start := time.Now()
			err := waitForSSHReadyWithProbeContext(ctx, probeCtx, &target, progress, "bootstrap", start, start.Add(5*time.Second))
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "--lease-id cbx_012345abcdef") {
				t.Fatalf("got %v", err)
			}
			if ExitCodeForError(err, 1) != 5 {
				t.Fatalf("exit=%d", ExitCodeForError(err, 1))
			}
			t.Log(err)
			if tc.name == "pending" {
				// Same target and identity, now ready. No allocation operation exists here.
				target.ReadyCheck = "exit 0"
				target.ReadinessStatus = ""
				if err := waitForSSHReady(checkpointWaitContext(t.Context(), time.Second), &target, io.Discard, "bootstrap", time.Hour); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCheckpointReadinessCancellation(t *testing.T) {
	target, _ := readinessWaitFixture(t, "cancel")
	ctx, cancel := context.WithCancelCause(checkpointWaitContext(t.Context(), time.Hour))
	cause := errors.New("operator interrupted wait")
	out := writerFunc(func(p []byte) (int, error) { cancel(cause); return len(p), nil })
	if err := waitForSSHReady(ctx, &target, out, "bootstrap", time.Hour); !errors.Is(err, cause) {
		t.Fatalf("got %v", err)
	}
}

func TestCheckpointReadinessLifetime(t *testing.T) {
	ctx := checkpointWaitContext(t.Context(), time.Hour)
	for _, tc := range []struct {
		name    string
		expires time.Time
		policy  *LeaseLifetimePolicy
		want    bool
	}{
		{"expired", time.Now().Add(-time.Second), nil, true},
		{"remaining", time.Now().Add(time.Hour), nil, false},
		{"enforced stop", time.Now().Add(time.Hour), &LeaseLifetimePolicy{Mode: "enforce", DeadlineAt: time.Now().Add(-time.Second).Format(time.RFC3339)}, true},
		{"extension", time.Now().Add(time.Hour), &LeaseLifetimePolicy{Mode: "enforce", DeadlineAt: time.Now().Add(-time.Second).Format(time.RFC3339), Extension: &LeaseLifetimeExtension{ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}}, false},
		{"report only", time.Now().Add(time.Hour), &LeaseLifetimePolicy{Mode: "report", DeadlineAt: time.Now().Add(-time.Second).Format(time.RFC3339)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckCheckpointReadinessLifetime(ctx, Server{Labels: map[string]string{"expires_at": tc.expires.Format(time.RFC3339Nano)}, LifetimePolicy: tc.policy})
			if (err != nil) != tc.want {
				t.Fatalf("got %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "state=lifetime_exhausted") {
				t.Fatal(err)
			}
		})
	}
	// A fresh observation from the existing lease watch replaces old idle expiry.
	expired := Server{Labels: map[string]string{"expires_at": time.Now().Add(-time.Second).Format(time.RFC3339Nano)}}
	ctx = WithCheckpointReadinessLifetime(ctx, expired)
	if err := CheckCheckpointReadinessLifetime(ctx, Server{Labels: map[string]string{"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339Nano)}}); err != nil {
		t.Fatal(err)
	}
}

type checkpointReadinessBackendFixture struct {
	*checkpointFixedForkBackend
	pending bool
	timeout time.Duration
}

func (b *checkpointReadinessBackendFixture) Acquire(ctx context.Context, req AcquireRequest) (LeaseTarget, error) {
	lease, err := b.checkpointFixedForkBackend.Acquire(ctx, req)
	if err != nil {
		return lease, err
	}
	policy, ok := checkpointReadinessFromContext(ctx)
	if !ok {
		return LeaseTarget{}, errors.New("missing fixed fork wait policy")
	}
	b.timeout = policy.Timeout
	if b.pending {
		return LeaseTarget{}, policy.failure("pending", "ownership-repair", policy.Timeout, time.Now())
	}
	return lease, nil
}

func TestCheckpointReadinessCommandJSONAndResume(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	backend := &checkpointReadinessBackendFixture{checkpointFixedForkBackend: &checkpointFixedForkBackend{checkpointForkReleaseBackend: &checkpointForkReleaseBackend{}}, pending: true}
	testAWSBackendOverride = backend
	t.Cleanup(func() { testAWSBackendOverride = nil })
	record := createCheckpointForkTestRecord(t, "chk_readiness_command", "")
	args := []string{record.ID, "--lease-id", "cbx_012345abcdef", "--slug", "fork-smoke", "--json"}
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	err := app.checkpointFork(t.Context(), args)
	if err == nil || !strings.Contains(err.Error(), "lease=cbx_012345abcdef checkpoint=") {
		t.Fatalf("got %v", err)
	}
	if stdout.Len() != 0 || backend.timeout != time.Hour || backend.creates != 1 || backend.releaseCount != 0 {
		t.Fatalf("pending output=%s timeout=%s creates=%d releases=%d", stdout.String(), backend.timeout, backend.creates, backend.releaseCount)
	}
	backend.pending = false
	args = append(args, "--wait-timeout", "90m")
	if err := app.checkpointFork(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	assertCheckpointForkJSON(t, stdout.Bytes(), record.ID, 1, "cbx_012345abcdef")
	if backend.creates != 1 || backend.releaseCount != 0 || backend.timeout != 90*time.Minute {
		t.Fatalf("replay creates=%d releases=%d timeout=%s", backend.creates, backend.releaseCount, backend.timeout)
	}
}

func TestCheckpointReadinessFlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"chk_test", "--wait-timeout", "0"},
		{"chk_test", "--wait-timeout", "-1m"},
		{"chk_test", "--wait-timeout", "5h"},
		{"chk_test", "--wait-timeout", "1m"},
	} {
		err := (App{Stdout: io.Discard, Stderr: io.Discard}).checkpointFork(t.Context(), args)
		if err == nil || ExitCodeForError(err, 1) != 2 || !strings.Contains(err.Error(), "--wait-timeout") {
			t.Fatalf("args=%v error=%v", args, err)
		}
	}
}

func TestCheckpointReadinessUsesLiveLeaseWatch(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh heartbeat supersedes old idle expiry", true: "lifetime exhaustion"}[expired], func(t *testing.T) {
			oldInterval := coordinatorLeaseWatchInterval
			coordinatorLeaseWatchInterval = 5 * time.Millisecond
			defer func() { coordinatorLeaseWatchInterval = oldInterval }()
			var calls atomic.Int32
			observed := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				select {
				case observed <- struct{}{}:
				default:
				}
				expiry := time.Now().Add(time.Hour)
				if expired {
					expiry = time.Now().Add(-time.Second)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: "cbx_012345abcdef", Provider: "aws", State: "active", ExpiresAt: expiry.Format(time.RFC3339Nano)}})
			}))
			defer server.Close()
			ctx := WithCheckpointReadinessLifetime(checkpointWaitContext(t.Context(), time.Hour), Server{Labels: map[string]string{"expires_at": time.Now().Add(-time.Second).Format(time.RFC3339Nano)}})
			ctx, cancel := context.WithCancelCause(ctx)
			defer cancel(nil)
			coord := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
			stop := startCoordinatorLeaseWatch(ctx, coord, "cbx_012345abcdef", cancel, io.Discard)
			if expired {
				select {
				case <-ctx.Done():
					if !strings.Contains(context.Cause(ctx).Error(), "lifetime_exhausted") {
						t.Fatal(context.Cause(ctx))
					}
				case <-time.After(time.Second):
					t.Error("watch did not stop exhausted lifetime")
				}
			} else {
				select {
				case <-ctx.Done():
					t.Error(context.Cause(ctx))
				case <-observed:
					select {
					case <-ctx.Done():
						t.Error(context.Cause(ctx))
					default:
					}
				case <-time.After(time.Second):
					t.Error("lease watch did not poll")
				}
			}
			stop()
			if calls.Load() == 0 {
				t.Fatal("no authority check")
			}
			count := calls.Load()
			time.Sleep(10 * time.Millisecond)
			if calls.Load() != count {
				t.Fatal("lease watch outlived the wait")
			}
		})
	}
}

func TestCheckpointReadinessCancellationPrecedesLifetime(t *testing.T) {
	ctx, cancel := context.WithCancel(checkpointWaitContext(t.Context(), time.Hour))
	cancel()
	if err := CheckCheckpointReadinessLifetime(ctx, Server{Labels: map[string]string{"expires_at": time.Now().Add(-time.Second).Format(time.RFC3339Nano)}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// A failure may develop after the initial status observation while the existing
// guest-side optimization is running. Its 30s slice must return to the observer,
// rather than consuming the extended invocation deadline with stale status.
func TestCheckpointReadinessFailureDuringGuestWait(t *testing.T) {
	target, dir := readinessWaitFixture(t, "timeout")
	target.ReadinessStatus = "fixture-status"
	script := `#!/bin/sh
for remote; do :; done
case "$remote" in
 fixture-status)
   if test -f "$CRABBOX_WAIT_POLLS"; then echo 'failed bootstrap'; else echo 'pending bootstrap'; fi
   exit 0;;
 'exit 0') exit 0;;
 *'sleep 0.25'*)
   case "$remote" in 'timeout 30s '*) touch "$CRABBOX_WAIT_POLLS"; exit 124;; *) exit 99;; esac;;
 *) exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer
	err := waitForSSHReady(checkpointWaitContext(t.Context(), 5*time.Second), &target, &progress, "bootstrap", time.Hour)
	if err == nil || !strings.Contains(err.Error(), "state=bootstrap_failed") || !strings.Contains(err.Error(), "phase=bootstrap") {
		t.Fatalf("dynamic terminal failure: %v\n%s", err, progress.String())
	}
	if _, err := os.Stat(os.Getenv("CRABBOX_WAIT_POLLS")); err != nil {
		t.Fatal("guest wait did not run its bounded slice", err)
	}
	t.Log(err)
}

func TestCheckpointReadinessStatusFollowsAuthenticatedFallback(t *testing.T) {
	target, dir := readinessWaitFixture(t, "timeout")
	workingPort := target.Port
	// Own and then close an ephemeral primary port, so TCP readiness must use
	// the fixture's existing listener on the fallback port.
	primary, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, target.Port, _ = net.SplitHostPort(primary.Addr().String())
	if err := primary.Close(); err != nil {
		t.Fatal(err)
	}
	target.FallbackPorts = []string{workingPort}
	target.ReadinessStatus = "fixture-status"
	t.Setenv("CRABBOX_WORKING_STATUS_PORT", workingPort)
	script := `#!/bin/sh
previous= port=
for arg; do
  if test "$previous" = -p; then port=$arg; fi
  previous=$arg
  remote=$arg
done
case "$remote" in
 fixture-status)
   if test "$port" != "$CRABBOX_WORKING_STATUS_PORT"; then exec sleep 30; fi
   echo 'failed bootstrap'; exit 0;;
 'exit 0') exit 0;;
 *) exit 124;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	configuredPort := target.Port
	err = waitForSSHReady(checkpointWaitContext(t.Context(), 12*time.Second), &target, io.Discard, "bootstrap", time.Hour)
	if err == nil || !strings.Contains(err.Error(), "state=bootstrap_failed") {
		t.Fatalf("fallback observer: %v", err)
	}
	if target.Port != configuredPort {
		t.Fatal("a status observation admitted the target's endpoint before readiness")
	}
}
