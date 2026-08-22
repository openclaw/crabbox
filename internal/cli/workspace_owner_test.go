package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeWorkspaceOwnerRemote struct {
	mu               sync.Mutex
	token            string
	expires          time.Time
	childAlive       bool
	failRenew        bool
	failRenewCount   int
	terminalRenewErr bool
	loseAcquireCount int
	legacyAcquire    bool
	acquireCalls     int
	ambiguousRelease bool
	blockBusyAcquire bool
	changed          chan struct{}
}

func newFakeWorkspaceOwnerRemote() *fakeWorkspaceOwnerRemote {
	return &fakeWorkspaceOwnerRemote{changed: make(chan struct{})}
}

func (f *fakeWorkspaceOwnerRemote) signalLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeWorkspaceOwnerRemote) Do(ctx context.Context, req workspaceOwnerRemoteRequest) (string, error) {
	for {
		f.mu.Lock()
		switch req.Action {
		case workspaceOwnerAcquire:
			f.acquireCalls++
			if f.legacyAcquire {
				f.mu.Unlock()
				return "LEGACY", nil
			}
			if f.token == "" {
				f.token = req.Token
				f.expires = time.Now().Add(req.TTL)
				f.signalLocked()
				if f.loseAcquireCount > 0 {
					f.loseAcquireCount--
					f.mu.Unlock()
					return "", errors.New("acquire response lost")
				}
				f.mu.Unlock()
				return "ACQUIRED", nil
			}
			if f.token == req.Token && time.Now().Before(f.expires) {
				f.expires = time.Now().Add(req.TTL)
				if f.loseAcquireCount > 0 {
					f.loseAcquireCount--
					f.mu.Unlock()
					return "", errors.New("acquire response lost")
				}
				f.mu.Unlock()
				return "ACQUIRED", nil
			}
			if time.Now().After(f.expires) {
				if f.childAlive {
					f.mu.Unlock()
					return "CHILD", nil
				}
				f.token = req.Token
				f.expires = time.Now().Add(req.TTL)
				f.signalLocked()
				f.mu.Unlock()
				return "RECOVERED", nil
			}
			if !f.blockBusyAcquire {
				f.mu.Unlock()
				return "BUSY", nil
			}
			changed := f.changed
			f.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-changed:
				continue
			}
		case workspaceOwnerRenew:
			if f.failRenewCount > 0 {
				f.failRenewCount--
				f.mu.Unlock()
				return "", errors.New("renew response lost")
			}
			if f.failRenew {
				f.mu.Unlock()
				return "", errors.New("renew transport lost")
			}
			if f.token != req.Token {
				err := error(nil)
				if f.terminalRenewErr {
					err = errors.New("remote exit 75")
				}
				f.mu.Unlock()
				return "MISMATCH", err
			}
			if !time.Now().Before(f.expires) {
				err := error(nil)
				if f.terminalRenewErr {
					err = errors.New("remote exit 75")
				}
				f.mu.Unlock()
				return "EXPIRED", err
			}
			f.expires = time.Now().Add(req.TTL)
			f.mu.Unlock()
			return "RENEWED", nil
		case workspaceOwnerInspect:
			if f.token != req.Token {
				f.mu.Unlock()
				return "MISMATCH", nil
			}
			if f.childAlive {
				f.mu.Unlock()
				return "CHILD", nil
			}
			f.mu.Unlock()
			return "OWNED", nil
		case workspaceOwnerRelease:
			if f.ambiguousRelease {
				f.mu.Unlock()
				return "", errors.New("release response lost")
			}
			if f.token != req.Token {
				f.mu.Unlock()
				return "MISMATCH", nil
			}
			if f.childAlive {
				f.mu.Unlock()
				return "CHILD", nil
			}
			f.token = ""
			f.signalLocked()
			f.mu.Unlock()
			return "RELEASED", nil
		default:
			f.mu.Unlock()
			return "AMBIGUOUS", nil
		}
	}
}

func acquireFakeWorkspaceOwner(t *testing.T, ctx context.Context, remote *fakeWorkspaceOwnerRemote, leaseID string) *workspaceOwner {
	t.Helper()
	owner, err := acquireWorkspaceOwnerWithTransport(ctx, SSHTarget{}, leaseID, &bytes.Buffer{}, remote, 250*time.Millisecond, 80*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("acquire workspace owner: %v", err)
	}
	return owner
}

func crashFakeWorkspaceOwner(t *testing.T, owner *workspaceOwner) {
	t.Helper()
	owner.closeOnce.Do(func() {
		close(owner.stop)
		<-owner.done
	})
}

func TestWorkspaceOwnerSerializesIndependentClientsAndRevisions(t *testing.T) {
	remote := newFakeWorkspaceOwnerRemote()
	remote.blockBusyAcquire = true
	ownerA := acquireFakeWorkspaceOwner(t, context.Background(), remote, "cbx_shared")

	type fakeWorkspace struct {
		sync.Mutex
		revision string
	}
	var workspace fakeWorkspace
	var active atomic.Int32
	var overlap atomic.Bool
	startedA := make(chan struct{})
	releaseA := make(chan struct{})
	done := make(chan error, 2)
	run := func(owner *workspaceOwner, revision string, started chan<- struct{}, release <-chan struct{}) {
		if active.Add(1) != 1 {
			overlap.Store(true)
		}
		defer active.Add(-1)
		workspace.Lock()
		workspace.revision = revision
		workspace.Unlock()
		if started != nil {
			close(started)
		}
		if release != nil {
			<-release
		}
		workspace.Lock()
		executed := workspace.revision
		workspace.Unlock()
		if executed != revision {
			done <- fmt.Errorf("executed revision %s, want %s", executed, revision)
			return
		}
		done <- nil
	}
	go run(ownerA, "revision-a", startedA, releaseA)
	<-startedA

	ownerBCh := make(chan *workspaceOwner, 1)
	errBCh := make(chan error, 1)
	go func() {
		ownerB, err := acquireWorkspaceOwnerWithTransport(context.Background(), SSHTarget{}, "cbx_shared", &bytes.Buffer{}, remote, time.Second, 80*time.Millisecond, 20*time.Millisecond)
		if err != nil {
			errBCh <- err
			return
		}
		ownerBCh <- ownerB
	}()
	select {
	case ownerB := <-ownerBCh:
		_ = ownerB.Close(context.Background())
		t.Fatal("second client acquired while the first lifecycle was active")
	case err := <-errBCh:
		t.Fatalf("second client failed instead of waiting: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseA)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := ownerA.Close(context.Background()); err != nil {
		t.Fatalf("release first owner: %v", err)
	}
	var ownerB *workspaceOwner
	select {
	case ownerB = <-ownerBCh:
	case err := <-errBCh:
		t.Fatalf("second client acquire: %v", err)
	case <-time.After(time.Second):
		t.Fatal("second client did not acquire after release")
	}
	go run(ownerB, "revision-b", nil, nil)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if overlap.Load() {
		t.Fatal("independent clients overlapped workspace lifecycle operations")
	}
	if err := ownerB.Close(context.Background()); err != nil {
		t.Fatalf("release second owner: %v", err)
	}
}

func TestWorkspaceOwnerCrashRecoveryHonorsWitnessedChild(t *testing.T) {
	remote := newFakeWorkspaceOwnerRemote()
	owner := acquireFakeWorkspaceOwner(t, context.Background(), remote, "cbx_child")
	crashFakeWorkspaceOwner(t, owner)
	remote.mu.Lock()
	remote.expires = time.Now().Add(-time.Second)
	remote.childAlive = true
	remote.mu.Unlock()

	_, err := acquireWorkspaceOwnerWithTransport(context.Background(), SSHTarget{}, "cbx_child", &bytes.Buffer{}, remote, 40*time.Millisecond, time.Second, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("live witnessed child takeover err=%v, want bounded refusal", err)
	}
	remote.mu.Lock()
	remote.childAlive = false
	remote.mu.Unlock()
	recovered := acquireFakeWorkspaceOwner(t, context.Background(), remote, "cbx_child")
	if err := recovered.Close(context.Background()); err != nil {
		t.Fatalf("release recovered owner: %v", err)
	}
}

func TestWorkspaceOwnerTokenAndTransportFailuresFailClosed(t *testing.T) {
	t.Run("token mismatch", func(t *testing.T) {
		remote := newFakeWorkspaceOwnerRemote()
		owner := acquireFakeWorkspaceOwner(t, context.Background(), remote, "cbx_token")
		remote.mu.Lock()
		remote.token = strings.Repeat("f", 64)
		remote.mu.Unlock()
		if err := owner.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "mismatch") {
			t.Fatalf("close err=%v, want token mismatch", err)
		}
	})

	t.Run("failed renew", func(t *testing.T) {
		remote := newFakeWorkspaceOwnerRemote()
		owner, err := acquireWorkspaceOwnerWithTransport(context.Background(), SSHTarget{}, "cbx_renew", &bytes.Buffer{}, remote, time.Second, 50*time.Millisecond, 5*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		remote.mu.Lock()
		remote.failRenew = true
		remote.mu.Unlock()
		select {
		case <-owner.Context().Done():
		case <-time.After(time.Second):
			t.Fatal("renewal failure did not cancel lifecycle context")
		}
		if err := owner.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "renewal failed closed") {
			t.Fatalf("close err=%v, want renewal failure", err)
		}
	})

	t.Run("ambiguous release", func(t *testing.T) {
		remote := newFakeWorkspaceOwnerRemote()
		owner := acquireFakeWorkspaceOwner(t, context.Background(), remote, "cbx_release")
		remote.mu.Lock()
		remote.ambiguousRelease = true
		remote.mu.Unlock()
		if err := owner.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "ambiguous remote state") {
			t.Fatalf("close err=%v, want ambiguous release", err)
		}
	})
}

func TestWorkspaceOwnerReconcilesAmbiguousAcquireAndRenewal(t *testing.T) {
	t.Run("response-loss acquire", func(t *testing.T) {
		remote := newFakeWorkspaceOwnerRemote()
		remote.loseAcquireCount = 1
		owner, err := acquireWorkspaceOwnerWithTransport(context.Background(), SSHTarget{}, "cbx_acquire_reconcile", &bytes.Buffer{}, remote, 2*time.Second, time.Second, 100*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if err := owner.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("legacy acquire fails once with recovery", func(t *testing.T) {
		remote := newFakeWorkspaceOwnerRemote()
		remote.legacyAcquire = true
		leaseID := "cbx_legacy_recovery"
		_, err := acquireWorkspaceOwnerWithTransport(context.Background(), SSHTarget{}, leaseID, &bytes.Buffer{}, remote, 2*time.Second, time.Second, 100*time.Millisecond)
		var exitErr ExitError
		if err == nil || !errors.As(err, &exitErr) || exitErr.Code != 7 {
			t.Fatalf("legacy acquire err=%v", err)
		}
		key := workspaceOwnerKey(leaseID)
		for _, want := range []string{leaseID, key, "~/.crabbox/workspace-owners/" + key + ".child", "quiesce or reboot", "PID/start identity", "remove only that child file"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("legacy acquire error missing %q: %v", want, err)
			}
		}
		if remote.acquireCalls != 1 {
			t.Fatalf("legacy acquire calls=%d want 1", remote.acquireCalls)
		}
	})

	t.Run("transient renewal", func(t *testing.T) {
		remote := newFakeWorkspaceOwnerRemote()
		owner, err := acquireWorkspaceOwnerWithTransport(context.Background(), SSHTarget{}, "cbx_renew_reconcile", &bytes.Buffer{}, remote, time.Second, 700*time.Millisecond, 50*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		remote.mu.Lock()
		remote.failRenewCount = 1
		remote.mu.Unlock()
		select {
		case <-owner.Context().Done():
			t.Fatalf("transient renewal canceled owner: %v", owner.Err())
		case <-time.After(450 * time.Millisecond):
		}
		if err := owner.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("deadline exhaustion", func(t *testing.T) {
		remote := newFakeWorkspaceOwnerRemote()
		owner, err := acquireWorkspaceOwnerWithTransport(context.Background(), SSHTarget{}, "cbx_renew_deadline", &bytes.Buffer{}, remote, time.Second, 120*time.Millisecond, 20*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		remote.mu.Lock()
		remote.failRenew = true
		remote.mu.Unlock()
		select {
		case <-owner.Context().Done():
		case <-time.After(time.Second):
			t.Fatal("renewal ambiguity outlived the confirmed deadline")
		}
		if err := owner.Err(); err == nil || !strings.Contains(err.Error(), "deadline exhausted") {
			t.Fatalf("renewal err=%v, want deadline exhaustion", err)
		}
		_ = owner.Close(context.Background())
	})

	for _, test := range []struct {
		name   string
		mutate func(*fakeWorkspaceOwnerRemote)
		want   string
	}{
		{name: "mismatch cancels immediately", mutate: func(remote *fakeWorkspaceOwnerRemote) {
			remote.token = strings.Repeat("f", 64)
			remote.terminalRenewErr = true
		}, want: "mismatch"},
		{name: "expiry cancels immediately", mutate: func(remote *fakeWorkspaceOwnerRemote) {
			remote.expires = time.Now().Add(-time.Second)
			remote.terminalRenewErr = true
		}, want: "expired"},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := newFakeWorkspaceOwnerRemote()
			owner, err := acquireWorkspaceOwnerWithTransport(context.Background(), SSHTarget{}, "cbx_renew_"+test.want, &bytes.Buffer{}, remote, time.Second, time.Second, 20*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			remote.mu.Lock()
			test.mutate(remote)
			remote.mu.Unlock()
			select {
			case <-owner.Context().Done():
			case <-time.After(200 * time.Millisecond):
				t.Fatalf("renewal %s did not cancel immediately", test.want)
			}
			if err := owner.Err(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("renewal err=%v, want %s", err, test.want)
			}
			_ = owner.Close(context.Background())
		})
	}
}

func TestWorkspaceOwnerAcquisitionBoundary(t *testing.T) {
	nonExclusive := newWatchTestBackend()
	if !shouldAcquireWorkspaceOwner(true, false, nonExclusive) {
		t.Fatal("a successful non-exclusive acquisition must acquire the workspace owner")
	}
	exclusive := runEnvProfileTestBackend{}
	tests := []struct {
		name      string
		acquired  bool
		mayRetain bool
		wantOwner bool
	}{
		{name: "fresh one-shot cleanup", acquired: true, wantOwner: false},
		{name: "fresh keep", acquired: true, mayRetain: true, wantOwner: true},
		{name: "fresh keep-on-failure", acquired: true, mayRetain: true, wantOwner: true},
		{name: "reused retained lease", acquired: false, wantOwner: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAcquireWorkspaceOwner(tt.acquired, tt.mayRetain, exclusive); got != tt.wantOwner {
				t.Fatalf("shouldAcquireWorkspaceOwner()=%t, want %t", got, tt.wantOwner)
			}
		})
	}
}

func TestAcquiredRunMayRetainLease(t *testing.T) {
	tests := []struct {
		name          string
		keep          bool
		keepOnFailure bool
		stopAfter     string
		want          bool
	}{
		{name: "default cleanup"},
		{name: "always cleanup", stopAfter: "always"},
		{name: "keep", keep: true, want: true},
		{name: "keep on failure", keepOnFailure: true, want: true},
		{name: "success policy may retain failure", stopAfter: "success", want: true},
		{name: "failure policy may retain success", stopAfter: "failure", want: true},
		{name: "never cleanup", stopAfter: "never", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := acquiredRunMayRetainLease(tt.keep, tt.keepOnFailure, tt.stopAfter); got != tt.want {
				t.Fatalf("acquiredRunMayRetainLease()=%t, want %t", got, tt.want)
			}
		})
	}
}

func TestWorkspaceOwnerContextWrapsEverySSHChild(t *testing.T) {
	owner := &workspaceOwner{target: SSHTarget{TargetOS: targetLinux}, key: strings.Repeat("a", 64), token: strings.Repeat("b", 64)}
	ctx := contextWithWorkspaceOwner(context.Background(), owner)
	prepared, err := prepareWorkspaceOwnerRemote(ctx, owner.target, "printf ok", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := prepared.command, remoteWorkspaceOwnerPOSIXWitness(owner.key, owner.token, "printf ok"); got != want {
		t.Fatalf("ordinary SSH child was not witnessed:\n%s", got)
	}
	if script := remoteWorkspaceOwnerPOSIXWitnessScript(owner.key, owner.token, "printf ok"); !strings.Contains(script, "sentinel_identity=$(ps -o lstart=") {
		t.Fatalf("ordinary SSH child witness lost identity fencing:\n%s", script)
	}
	inputSize := int64(0)
	prepared, err = prepareWorkspaceOwnerRemote(ctx, owner.target, "cat", &inputSize)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := prepared.command, remoteWorkspaceOwnerPOSIXWitness(owner.key, owner.token, "cat", true); got != want {
		t.Fatalf("input SSH child did not preserve stdin:\n%s", got)
	}
	if script := remoteWorkspaceOwnerPOSIXWitnessScript(owner.key, owner.token, "cat", true); !strings.Contains(script, `cat >"$run_dir/input"`) || !strings.Contains(script, `<"$run_dir/input"`) {
		t.Fatalf("input SSH child witness lost stdin preservation:\n%s", script)
	}
	prepared, err = prepareWorkspaceOwnerRemote(contextWithoutWorkspaceOwner(ctx), owner.target, "printf raw", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := prepared.command; got != "printf raw" {
		t.Fatalf("owner bypass wrapped protocol-internal command: %q", got)
	}
}

func TestWorkspaceOwnerLifecycleBoundaryMatrix(t *testing.T) {
	for _, lifecycle := range []string{
		"fingerprint skip",
		"normal sync",
		"full resync",
		"no sync",
		"sync only",
		"fresh pr",
		"actions hydration",
		"command",
		"results and artifacts",
		"failure bundle",
		"pool scrub and return",
		"watch iteration",
	} {
		t.Run(lifecycle, func(t *testing.T) {
			if !shouldAcquireWorkspaceOwner(false, false, nil) {
				t.Fatalf("reused %s path bypassed workspace ownership", lifecycle)
			}
		})
	}
}

func TestWorkspaceOwnerSerializesStaticRunAndStandaloneActionsHydration(t *testing.T) {
	if !shouldAcquireWorkspaceOwner(true, false, testStaticSSHBackend{}) {
		t.Fatal("static SSH acquisition bypassed workspace ownership")
	}
	remote := newFakeWorkspaceOwnerRemote()
	remote.blockBusyAcquire = true
	runOwner := acquireFakeWorkspaceOwner(t, context.Background(), remote, "cbx_run_hydrate")
	hydrateOwner := make(chan *workspaceOwner, 1)
	hydrateErr := make(chan error, 1)
	go func() {
		owner, err := acquireWorkspaceOwnerWithTransport(context.Background(), SSHTarget{}, "cbx_run_hydrate", &bytes.Buffer{}, remote, time.Second, time.Second, 100*time.Millisecond)
		if err != nil {
			hydrateErr <- err
			return
		}
		hydrateOwner <- owner
	}()
	select {
	case owner := <-hydrateOwner:
		_ = owner.Close(context.Background())
		t.Fatal("standalone Actions hydration overlapped the normal run owner")
	case err := <-hydrateErr:
		t.Fatalf("standalone Actions hydration failed instead of contending: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := runOwner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case owner := <-hydrateOwner:
		if err := owner.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	case err := <-hydrateErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("standalone Actions hydration did not acquire after the run released")
	}
}

func TestWorkspaceOwnerProtocolGeneration(t *testing.T) {
	leaseID := "cbx_raw-lease-must-not-appear"
	key := workspaceOwnerKey(leaseID)
	token := strings.Repeat("a", 64)
	req := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: time.Minute}

	posixTransport := remoteWorkspaceOwnerCommand(SSHTarget{TargetOS: targetLinux}, req)
	wsl := remoteWorkspaceOwnerCommand(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, req)
	if posixTransport != wsl {
		t.Fatal("POSIX and WSL2 must use the same owner protocol")
	}
	if !strings.HasPrefix(posixTransport, "exec /bin/sh -c '") || strings.Contains(posixTransport, "\n") || strings.Count(posixTransport, "'") != 2 {
		t.Fatalf("POSIX owner protocol must use the portable launcher: %q", posixTransport[:min(len(posixTransport), 80)])
	}
	posix := remoteWorkspaceOwnerPOSIX(req)
	for _, want := range []string{".crabbox/workspace-owners", key + ".gate", "$key.owner", "$key.child", "flock -x -w 0", "lockf -t 0", "ps -o lstart=", "RECOVERED", "MISMATCH", "EXPIRED", "LEGACY", "AMBIGUOUS", `[ "$state_expiry" -gt "$(date +%s)" ]`} {
		if !strings.Contains(posix, want) {
			t.Fatalf("POSIX protocol missing %q:\n%s", want, posix)
		}
	}
	if strings.Contains(posix, leaseID) {
		t.Fatalf("POSIX protocol exposed raw lease ID: %s", posix)
	}

	windows := remoteWorkspaceOwnerWindows(req)
	for _, want := range []string{".crabbox\\workspace-owners", "$key = '" + key + "'", "$key + \".owner\"", "$key + \".child\"", "[Diagnostics.Process]::GetProcessById", "return \"ambiguous\"", "StartTime.ToUniversalTime().Ticks", "RECOVERED", "MISMATCH", "EXPIRED", "AMBIGUOUS", "$current.Expiry -le [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()"} {
		if !strings.Contains(windows, want) {
			t.Fatalf("Windows protocol missing %q:\n%s", want, windows)
		}
	}
	if strings.Contains(windows, leaseID) {
		t.Fatalf("Windows protocol exposed raw lease ID: %s", windows)
	}
	posixWitnessTransport := remoteWorkspaceOwnerPOSIXWitness(key, token, "printf ok")
	if !strings.HasPrefix(posixWitnessTransport, "exec /bin/sh -c '") || strings.Contains(posixWitnessTransport, "\n") || strings.Count(posixWitnessTransport, "'") != 2 {
		t.Fatalf("POSIX child witness must use the portable launcher: %q", posixWitnessTransport[:min(len(posixWitnessTransport), 80)])
	}
	posixWitness := remoteWorkspaceOwnerPOSIXWitnessScript(key, token, "printf ok")
	for _, want := range []string{"exec setsid /bin/sh", "exec perl -MPOSIX", "supervisor_setsid=setsid", "child_pgid=$(ps -o pgid=", "sentinel_identity=$(ps -o lstart=", "owner_expiry=$(sed -n", "owner_expiry", "date +%s", `v2\n%s\n%s\n%s\n`, "mv \"$child_tmp\" \"$child\"", `trap "" HUP TERM`, "exec 3<>", "kill -TERM 0", "kill -KILL 0", "expire() {", "|| expire", `done\n`, "ps -eo pid=,pgid=,stat=", "$3 !~ /Z/", "install_body=", `flock -x -w 5 "$gate" /bin/sh -c "$install_body"`, "supervisor-ready", "touch \"$start\"", "wait \"$child_pid\"", "rm -f \"$child\""} {
		if !strings.Contains(posixWitness, want) {
			t.Fatalf("POSIX child witness missing %q:\n%s", want, posixWitness)
		}
	}
	if strings.Contains(posixWitness, `kill -TERM -"$child_pid"`) || strings.Contains(posixWitness, `kill -KILL -"$child_pid"`) {
		t.Fatal("POSIX supervisor still signals the workload group externally")
	}
	if strings.Contains(posixWitness, `kill "$child_pid" "$sentinel_pid"`) || !strings.Contains(posixWitness, `[ "$sentinel_pid" -gt 1 ]`) {
		t.Fatal("POSIX startup cleanup can signal an invalid sentinel PID")
	}
	if strings.Index(posixWitness, `mv "$child_tmp" "$child"`) > strings.Index(posixWitness, `touch "$start"`) {
		t.Fatal("POSIX payload barrier opens before the child record is published")
	}
	if strings.Index(posixWitness, "nohup /bin/sh") > strings.Index(posixWitness, `touch "$start"`) {
		t.Fatal("POSIX payload barrier opens before the durable supervisor starts")
	}
	windowsWitness := remoteWorkspaceOwnerWindowsWitness(key, token, "Write-Output ok", nil)
	for _, want := range []string{"Start-Process", "$null = $process.Handle", "StartTime.ToUniversalTime().Ticks", "Read-Expiry", "(Read-Expiry) -le [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()", "Move-Item -LiteralPath $tmp -Destination $child", "$payloadExitCode = $global:LASTEXITCODE", "if (-not $payloadSucceeded) { exit 1 }", "$process.WaitForExit()", "$supervisor.WaitForExit()", "Stop-NewWitness 74", "LimitFlags=0x2000", "OpenProcess(0x1101", "GetProcessTimes", "AssignProcessToJobObject", "[CrabboxJob]::Attach($pidValue, [Int64]$identity); if ($job -eq [IntPtr]::Zero)", "[CrabboxJob]::Active($job)", "supervisor.ready", "[CrabboxJob]::CloseHandle($job)", "Remove-Item -LiteralPath $child", "Remove-Item -LiteralPath $runDir -Recurse"} {
		if !strings.Contains(windowsWitness, want) {
			t.Fatalf("Windows child witness missing %q:\n%s", want, windowsWitness)
		}
	}
	if strings.Index(windowsWitness, "GetProcessTimes") > strings.Index(windowsWitness, "AssignProcessToJobObject") {
		t.Fatal("Windows supervisor assigns a PID before validating its start identity")
	}
	if strings.Contains(windowsWitness, "taskkill.exe") {
		t.Fatal("Windows supervisor bypasses its identity-bound job object")
	}
	if !strings.Contains(windowsWitness, `@([string]$supervisor.Id, $supervisorIdentity)`) || strings.Contains(windowsWitness, `@([string]$process.Id, $identity)`) {
		t.Fatal("Windows child record does not retain the live supervisor identity")
	}
	if strings.Index(windowsWitness, `$supervisor = Start-Process`) > strings.Index(windowsWitness, "Move-Item -LiteralPath $tmp -Destination $child") {
		t.Fatal("Windows child record is published before the durable supervisor starts")
	}
	windowsInputSize := int64(len("input"))
	windowsInputWitness := remoteWorkspaceOwnerWindowsWitness(key, token, "Write-Output ok", &windowsInputSize)
	for _, want := range []string{"$remaining = [Int64]5", "$stdin.Read($buffer, 0, $readSize)", "-RedirectStandardInput $inputPath", "[IO.FileShare]::None"} {
		if !strings.Contains(windowsInputWitness, want) {
			t.Fatalf("Windows input witness missing %q:\n%s", want, windowsInputWitness)
		}
	}
}

func TestWorkspaceOwnerNativeWindowsCommandLengthBaseline(t *testing.T) {
	key := workspaceOwnerKey("cbx_windows_command_length")
	token := strings.Repeat("a", 64)
	req := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: time.Minute}
	acquire := remoteWorkspaceOwnerCommand(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, req)
	name := key + ".witness." + token + "." + strings.Repeat("b", 32) + ".ps1"
	commands := map[string]string{
		"owner acquire":       acquire,
		"large witness stage": remoteWorkspaceOwnerWindowsStageWitnessCommand(key, token, name, 20_000),
		"large witness run":   remoteWorkspaceOwnerWindowsRunWitnessCommand(name),
		"witness cleanup":     remoteWorkspaceOwnerWindowsCleanupWitnessCommand(name),
		"background witness":  remoteWorkspaceOwnerWindowsStartBackgroundWitnessCommand(name),
	}
	for label, command := range commands {
		t.Logf("%s command length=%d", label, len(command))
		if len(command) >= 8191 {
			t.Fatalf("%s command length=%d exceeds cmd.exe limit", label, len(command))
		}
		if strings.Contains(command, strings.Repeat("Write-Output 'large witness'\n", 512)) {
			t.Fatalf("%s embeds the large witness payload", label)
		}
	}
}

func installWorkspaceOwnerRecordingSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "ssh")
	script := `#!/bin/sh
log_dir=$CRABBOX_OWNER_SSH_LOG_DIR
count=0
if [ -f "$log_dir/count" ]; then read count < "$log_dir/count"; fi
count=$((count + 1))
printf '%s' "$count" > "$log_dir/count"
command=
for arg do command=$arg; done
printf '%s' "$command" > "$log_dir/$count.command"
/bin/cat > "$log_dir/$count.stdin"
if [ "${CRABBOX_OWNER_SSH_RETRY_CALL:-}" = "$count" ]; then
  printf '%s' "${CRABBOX_OWNER_SSH_RETRY_STDOUT:-}"
  printf '%s' "${CRABBOX_OWNER_SSH_RETRY_STDERR:-failed port diagnostic}" >&2
  exit 255
fi
if [ "${CRABBOX_OWNER_SSH_FAIL_CALL:-}" = "$count" ]; then
  printf '%s' "${CRABBOX_OWNER_SSH_FAIL_STDOUT:-}"
  printf '%s' "${CRABBOX_OWNER_SSH_FAIL_STDERR:-}" >&2
  exit "${CRABBOX_OWNER_SSH_FAIL_CODE:-7}"
fi
printf '%s' "${CRABBOX_OWNER_SSH_SUCCESS_STDERR:-}" >&2
if [ -n "${CRABBOX_OWNER_SSH_SUCCESS_STDOUT:-}" ]; then printf '%s' "$CRABBOX_OWNER_SSH_SUCCESS_STDOUT"; exit 0; fi
if /usr/bin/grep -Fq "\$action = 'acquire'" "$log_dir/$count.stdin"; then printf ACQUIRED; fi
if /usr/bin/grep -Fq "\$action = 'renew'" "$log_dir/$count.stdin"; then printf RENEWED; fi
if /usr/bin/grep -Fq "\$action = 'inspect'" "$log_dir/$count.stdin"; then printf OWNED; fi
if /usr/bin/grep -Fq "\$action = 'release'" "$log_dir/$count.stdin"; then printf RELEASED; fi
`
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_OWNER_SSH_LOG_DIR", dir)
	return dir
}

func readWorkspaceOwnerSSHCall(t *testing.T, dir string, index int) (string, string) {
	t.Helper()
	command, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(index)+".command"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(index)+".stdin"))
	if err != nil {
		t.Fatal(err)
	}
	return string(command), string(input)
}

func TestWorkspaceOwnerSSHProtocolIgnoresSuccessfulWarnings(t *testing.T) {
	tests := []struct {
		name   string
		target SSHTarget
	}{
		{name: "POSIX", target: SSHTarget{TargetOS: targetLinux}},
		{name: "WSL2", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}},
		{name: "native Windows", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			installWorkspaceOwnerRecordingSSH(t)
			t.Setenv("CRABBOX_OWNER_SSH_SUCCESS_STDOUT", "ACQUIRED")
			t.Setenv("CRABBOX_OWNER_SSH_SUCCESS_STDERR", "** WARNING: connection is not using a post-quantum key exchange algorithm.\n")
			test.target.User, test.target.Host, test.target.Port = "crabbox", "127.0.0.1", "22"
			got, err := (sshWorkspaceOwnerTransport{target: test.target}).Do(context.Background(), workspaceOwnerRemoteRequest{
				Action: workspaceOwnerAcquire,
				Key:    workspaceOwnerKey("cbx_warning_" + test.name),
				Token:  strings.Repeat("a", 64),
				TTL:    time.Minute,
			})
			if err != nil || got != "ACQUIRED" {
				t.Fatalf("response=%q err=%v", got, err)
			}
		})
	}
}

func TestWorkspaceOwnerSSHProtocolFailurePreservesResponseAndSafeDiagnostic(t *testing.T) {
	tests := []struct {
		name   string
		target SSHTarget
	}{
		{name: "POSIX", target: SSHTarget{TargetOS: targetLinux}},
		{name: "WSL2", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}},
		{name: "native Windows", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			installWorkspaceOwnerRecordingSSH(t)
			secret := strings.Repeat("s", 80)
			t.Setenv("CRABBOX_OWNER_SSH_FAIL_CALL", "1")
			t.Setenv("CRABBOX_OWNER_SSH_FAIL_CODE", "23")
			t.Setenv("CRABBOX_OWNER_SSH_FAIL_STDOUT", "MISMATCH")
			t.Setenv("CRABBOX_OWNER_SSH_FAIL_STDERR", strings.Repeat("x", 190)+" Authorization: Bearer "+secret+" "+strings.Repeat("z", 300)+"\n")
			test.target.User, test.target.Host, test.target.Port = "crabbox", "127.0.0.1", "22"
			got, err := (sshWorkspaceOwnerTransport{target: test.target}).Do(context.Background(), workspaceOwnerRemoteRequest{
				Action: workspaceOwnerRenew,
				Key:    workspaceOwnerKey("cbx_failure_" + test.name),
				Token:  strings.Repeat("b", 64),
				TTL:    time.Minute,
			})
			if got != "MISMATCH" || err == nil || exitCode(err) != 23 {
				t.Fatalf("response=%q err=%v exit=%d", got, err, exitCode(err))
			}
			if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), diagnosticRedaction) {
				t.Fatalf("failure diagnostic was not redacted: %q", err)
			}
			if detail := strings.TrimPrefix(err.Error(), "exit status 23: "); len(detail) > 243 {
				t.Fatalf("failure detail length=%d want <=243: %q", len(detail), detail)
			}
		})
	}
}

func TestWorkspaceOwnerSSHProtocolFallbackDoesNotContaminateSuccess(t *testing.T) {
	tests := []struct {
		name   string
		target SSHTarget
	}{
		{name: "POSIX", target: SSHTarget{TargetOS: targetLinux}},
		{name: "WSL2", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}},
		{name: "native Windows", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := installWorkspaceOwnerRecordingSSH(t)
			t.Setenv("CRABBOX_OWNER_SSH_RETRY_CALL", "1")
			t.Setenv("CRABBOX_OWNER_SSH_RETRY_STDOUT", "MISMATCH")
			t.Setenv("CRABBOX_OWNER_SSH_RETRY_STDERR", "first port failed")
			t.Setenv("CRABBOX_OWNER_SSH_SUCCESS_STDOUT", "ACQUIRED")
			t.Setenv("CRABBOX_OWNER_SSH_SUCCESS_STDERR", "second port warning")
			test.target.User, test.target.Host, test.target.Port = "crabbox", "127.0.0.1", "2222"
			test.target.FallbackPorts = []string{"22"}
			got, err := (sshWorkspaceOwnerTransport{target: test.target}).Do(context.Background(), workspaceOwnerRemoteRequest{
				Action: workspaceOwnerAcquire,
				Key:    workspaceOwnerKey("cbx_fallback_" + test.name),
				Token:  strings.Repeat("c", 64),
				TTL:    time.Minute,
			})
			if err != nil || got != "ACQUIRED" {
				t.Fatalf("fallback response=%q err=%v", got, err)
			}
			if count, readErr := os.ReadFile(filepath.Join(dir, "count")); readErr != nil || string(count) != "2" {
				t.Fatalf("SSH call count=%q err=%v", count, readErr)
			}
		})
	}
}

func TestWorkspaceOwnerNativeWindowsProtocolStreamsScriptsOverStdin(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	transport := sshWorkspaceOwnerTransport{target: target}
	key := workspaceOwnerKey("cbx_windows_protocol_transport")
	token := strings.Repeat("c", 64)
	actions := []struct {
		action workspaceOwnerAction
		want   string
	}{
		{workspaceOwnerAcquire, "ACQUIRED"},
		{workspaceOwnerRenew, "RENEWED"},
		{workspaceOwnerInspect, "OWNED"},
		{workspaceOwnerRelease, "RELEASED"},
	}
	for i, test := range actions {
		got, err := transport.Do(context.Background(), workspaceOwnerRemoteRequest{Action: test.action, Key: key, Token: token, TTL: time.Minute})
		if err != nil || got != test.want {
			t.Fatalf("%s response=%q err=%v", test.action, got, err)
		}
		command, input := readWorkspaceOwnerSSHCall(t, dir, i+1)
		if command != windowsPowerShellStdinScriptCommand(len([]byte(input))) {
			t.Fatalf("%s command=%q", test.action, command)
		}
		if len(command) >= 8191 {
			t.Fatalf("%s command length=%d exceeds cmd.exe limit", test.action, len(command))
		}
		for _, want := range []string{"$action = " + psQuote(string(test.action)), "$key = " + psQuote(key), "$token = " + psQuote(token)} {
			if !strings.Contains(input, want) {
				t.Fatalf("%s stdin script missing %q", test.action, want)
			}
		}
		if strings.Contains(command, "$action") || strings.Contains(command, key) || strings.Contains(command, token) {
			t.Fatalf("%s script leaked into SSH command argv", test.action)
		}
	}
}

func TestWorkspaceOwnerWSL2ProtocolStreamsScriptsOverStdin(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	transport := sshWorkspaceOwnerTransport{target: target}
	key := workspaceOwnerKey("cbx_wsl2_protocol_transport")
	token := strings.Repeat("6", 64)
	actions := []struct {
		action workspaceOwnerAction
		want   string
	}{
		{workspaceOwnerAcquire, "ACQUIRED"},
		{workspaceOwnerRenew, "RENEWED"},
		{workspaceOwnerInspect, "OWNED"},
		{workspaceOwnerRelease, "RELEASED"},
	}
	for i, test := range actions {
		req := workspaceOwnerRemoteRequest{Action: test.action, Key: key, Token: token, TTL: time.Minute}
		t.Setenv("CRABBOX_OWNER_SSH_SUCCESS_STDOUT", test.want)
		got, err := transport.Do(context.Background(), req)
		if err != nil || got != test.want {
			t.Fatalf("%s response=%q err=%v", test.action, got, err)
		}
		command, input := readWorkspaceOwnerSSHCall(t, dir, i+1)
		wantInput := remoteWorkspaceOwnerCommand(target, req)
		if input != wantInput {
			t.Fatalf("%s stdin did not preserve the owner script", test.action)
		}
		if command != wsl2StdinScriptCommandWithWaitTimeout(len(wantInput), 0) {
			t.Fatalf("%s command=%q", test.action, command)
		}
		if len(command) >= 8191 {
			t.Fatalf("%s command length=%d exceeds cmd.exe limit", test.action, len(command))
		}
		if strings.Contains(command, wantInput) || strings.Contains(command, key) || strings.Contains(command, token) {
			t.Fatalf("%s script or secret leaked into SSH command argv", test.action)
		}
	}
}

func TestWorkspaceOwnerWSL2ProtocolReplaysIdenticalFallbackInput(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	t.Setenv("CRABBOX_OWNER_SSH_RETRY_CALL", "1")
	t.Setenv("CRABBOX_OWNER_SSH_SUCCESS_STDOUT", "ACQUIRED")
	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "2222", FallbackPorts: []string{"22"}, TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	req := workspaceOwnerRemoteRequest{
		Action: workspaceOwnerAcquire,
		Key:    workspaceOwnerKey("cbx_wsl2_protocol_fallback"),
		Token:  strings.Repeat("7", 64),
		TTL:    time.Minute,
	}
	got, err := (sshWorkspaceOwnerTransport{target: target}).Do(context.Background(), req)
	if err != nil || got != "ACQUIRED" {
		t.Fatalf("fallback response=%q err=%v", got, err)
	}
	wantInput := remoteWorkspaceOwnerCommand(target, req)
	wantCommand := wsl2StdinScriptCommandWithWaitTimeout(len(wantInput), 0)
	for _, index := range []int{1, 2} {
		command, input := readWorkspaceOwnerSSHCall(t, dir, index)
		if command != wantCommand || input != wantInput {
			t.Fatalf("fallback attempt %d command_match=%t input_match=%t", index, command == wantCommand, input == wantInput)
		}
	}
}

func TestWorkspaceOwnerNativeWindowsProtocolKeepsOnlySuccessfulFallbackOutput(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	t.Setenv("CRABBOX_OWNER_SSH_RETRY_CALL", "1")
	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "2222", FallbackPorts: []string{"22"}, TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	transport := sshWorkspaceOwnerTransport{target: target}
	key := workspaceOwnerKey("cbx_windows_protocol_fallback")
	token := strings.Repeat("7", 64)
	got, err := transport.Do(context.Background(), workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: time.Minute})
	if err != nil || got != "ACQUIRED" {
		t.Fatalf("fallback response=%q err=%v", got, err)
	}
	for _, index := range []int{1, 2} {
		command, input := readWorkspaceOwnerSSHCall(t, dir, index)
		if command != windowsPowerShellStdinScriptCommand(len([]byte(input))) || !strings.Contains(input, "$action = 'acquire'") {
			t.Fatalf("fallback attempt %d command=%q", index, command)
		}
	}
}

func TestWorkspaceOwnerNativeWindowsAmbiguousStageCleansExactWitness(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_CALL", "1")
	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", FallbackPorts: []string{"2222"}, TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	owner := &workspaceOwner{target: target, key: workspaceOwnerKey("cbx_windows_ambiguous_stage"), token: strings.Repeat("8", 64)}
	ctx := contextWithWorkspaceOwner(context.Background(), owner)
	_, err := prepareWorkspaceOwnerRemote(ctx, target, "Write-Output ambiguous-stage", nil)
	if err == nil || exitCode(err) != 7 || !strings.Contains(err.Error(), "stage native Windows workspace witness") {
		t.Fatalf("ambiguous stage err=%v", err)
	}

	stageCommand, stagedScript := readWorkspaceOwnerSSHCall(t, dir, 1)
	if !strings.Contains(stagedScript, base64.StdEncoding.EncodeToString([]byte("Write-Output ambiguous-stage"))) {
		t.Fatal("staging failure did not follow a complete remote witness write")
	}
	stage := decodePowerShellCommand(t, stageCommand)
	prefix := owner.key + ".witness." + owner.token + "."
	start := strings.Index(stage, prefix)
	if start < 0 {
		t.Fatalf("stage command missing witness prefix %q", prefix)
	}
	end := strings.Index(stage[start:], ".ps1")
	if end < 0 {
		t.Fatalf("stage command missing witness suffix: %s", stage)
	}
	name := stage[start : start+end+len(".ps1")]
	cleanupCommand, cleanupInput := readWorkspaceOwnerSSHCall(t, dir, 2)
	if cleanupCommand != remoteWorkspaceOwnerWindowsCleanupWitnessCommand(name) || cleanupInput != "" {
		t.Fatalf("cleanup command=%q stdin=%q want exact witness=%q", cleanupCommand, cleanupInput, name)
	}
	count, readErr := os.ReadFile(filepath.Join(dir, "count"))
	if readErr != nil || string(count) != "2" {
		t.Fatalf("SSH call count=%q err=%v; want one failed stage and one cleanup without fallback", count, readErr)
	}
}

func TestWorkspaceOwnerCanceledCleanupUsesShortDetachedBudget(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	prepared := workspaceOwnerRemotePreparation{cleanup: "cleanup", name: "witness.ps1"}
	started := time.Now()
	var observedBudget time.Duration
	err := prepared.closeWithRunner(parent, SSHTarget{}, func(ctx context.Context, _ SSHTarget, command string) error {
		if command != prepared.cleanup {
			t.Fatalf("cleanup command=%q want %q", command, prepared.cleanup)
		}
		if ctx.Err() != nil {
			t.Fatalf("cleanup inherited caller cancellation: %v", ctx.Err())
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("cleanup context has no deadline")
		}
		observedBudget = time.Until(deadline)
		<-ctx.Done()
		return ctx.Err()
	})
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup err=%v want deadline exceeded", err)
	}
	if observedBudget <= 0 || observedBudget > workspaceOwnerCanceledCleanupTimeout+250*time.Millisecond {
		t.Fatalf("cleanup budget=%s want at most %s", observedBudget, workspaceOwnerCanceledCleanupTimeout)
	}
	if elapsed > workspaceOwnerCanceledCleanupTimeout+2*time.Second {
		t.Fatalf("cancelled cleanup elapsed=%s exceeded short budget %s", elapsed, workspaceOwnerCanceledCleanupTimeout)
	}
	if observedBudget >= workspaceOwnerCleanupTimeout {
		t.Fatalf("cancelled cleanup inherited normal timeout %s", workspaceOwnerCleanupTimeout)
	}
}

func TestWorkspaceOwnerNativeWindowsWitnessStagesRawScriptAndPreservesInput(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	owner := &workspaceOwner{target: target, key: workspaceOwnerKey("cbx_windows_witness_transport"), token: strings.Repeat("d", 64)}
	ctx := contextWithWorkspaceOwner(context.Background(), owner)
	payload := strings.Repeat("Write-Output 'large witness'\n", 512)
	userInput := "preserved user stdin\n"
	if err := runSSHInput(ctx, target, payload, strings.NewReader(userInput), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	stageCommand, stagedScript := readWorkspaceOwnerSSHCall(t, dir, 1)
	stage := decodePowerShellCommand(t, stageCommand)
	for _, want := range []string{"[IO.FileMode]::CreateNew", "[IO.FileShare]::None", "$stdin.Read($buffer, 0, $readSize)", "staged workspace witness length is ambiguous", owner.key, owner.token} {
		if !strings.Contains(stage, want) {
			t.Fatalf("stage command missing %q", want)
		}
	}
	encodedPayload := base64.StdEncoding.EncodeToString([]byte(payload))
	if !strings.Contains(stagedScript, encodedPayload) {
		t.Fatal("raw staged witness omitted the large remote payload")
	}
	for _, want := range []string{"$childScript = Join-Path $runDir \"child.ps1\"", "-File", "Remove-Item -LiteralPath $runDir"} {
		if !strings.Contains(stagedScript, want) {
			t.Fatalf("staged witness missing %q", want)
		}
	}
	if strings.Contains(stagedScript, "-EncodedCommand") {
		t.Fatal("staged witness still launches its child through EncodedCommand")
	}

	runCommand, runInput := readWorkspaceOwnerSSHCall(t, dir, 2)
	run := decodePowerShellCommand(t, runCommand)
	for _, want := range []string{"& powershell.exe", "-File $path", "finally", "Remove-Item -LiteralPath $path"} {
		if !strings.Contains(run, want) {
			t.Fatalf("run command missing %q", want)
		}
	}
	if runInput != userInput {
		t.Fatalf("witnessed command stdin=%q want=%q", runInput, userInput)
	}

	count, err := os.ReadFile(filepath.Join(dir, "count"))
	if err != nil || string(count) != "2" {
		t.Fatalf("SSH call count=%q err=%v; successful self-cleaning run must not open a cleanup connection", count, err)
	}
	for label, command := range map[string]string{"stage": stageCommand, "run": runCommand} {
		if len(command) >= 8191 {
			t.Fatalf("%s command length=%d exceeds cmd.exe limit", label, len(command))
		}
		if strings.Contains(command, payload) {
			t.Fatalf("%s command embeds the large witness payload", label)
		}
	}
}

func TestWorkspaceOwnerNativeWindowsWitnessFallsBackToCleanupAfterFailure(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_CALL", "2")
	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	owner := &workspaceOwner{target: target, key: workspaceOwnerKey("cbx_windows_witness_cleanup"), token: strings.Repeat("9", 64)}
	ctx := contextWithWorkspaceOwner(context.Background(), owner)
	if err := runSSHInput(ctx, target, "Write-Output fail", strings.NewReader("input"), io.Discard, io.Discard); err == nil {
		t.Fatal("failed witnessed command returned success")
	}
	cleanupCommand, cleanupInput := readWorkspaceOwnerSSHCall(t, dir, 3)
	cleanup := decodePowerShellCommand(t, cleanupCommand)
	if !strings.Contains(cleanup, "Remove-Item -LiteralPath $path") || cleanupInput != "" {
		t.Fatalf("fallback cleanup command=%q stdin=%q", cleanup, cleanupInput)
	}
}

func TestWorkspaceOwnerNativeWindowsBackgroundWitnessStagesSelfCleaningScript(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	owner := &workspaceOwner{target: target, key: workspaceOwnerKey("cbx_windows_background_transport"), token: strings.Repeat("e", 64)}
	payload := strings.Repeat("Write-Output background\n", 512)
	if _, err := runWorkspaceOwnerBackgroundOutput(context.Background(), target, owner, payload); err != nil {
		t.Fatal(err)
	}
	_, stagedScript := readWorkspaceOwnerSSHCall(t, dir, 1)
	if !strings.Contains(stagedScript, base64.StdEncoding.EncodeToString([]byte(payload))) || !strings.Contains(stagedScript, "$selfPath") || !strings.Contains(stagedScript, "finally") {
		t.Fatal("background witness was not staged with self-cleanup")
	}
	backgroundCommand, backgroundInput := readWorkspaceOwnerSSHCall(t, dir, 2)
	background := decodePowerShellCommand(t, backgroundCommand)
	for _, want := range []string{"Start-Process", "-File", "-WindowStyle Hidden", "Write-Output $process.Id"} {
		if !strings.Contains(background, want) {
			t.Fatalf("background command missing %q", want)
		}
	}
	if backgroundInput != "" || len(backgroundCommand) >= 8191 || strings.Contains(backgroundCommand, payload) {
		t.Fatalf("background command length=%d stdin=%q", len(backgroundCommand), backgroundInput)
	}
}

func TestWorkspaceOwnerNativeWindowsStreamPathsUseStagedWitnesses(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	owner := &workspaceOwner{target: target, key: workspaceOwnerKey("cbx_windows_stream_transport"), token: strings.Repeat("f", 64)}
	ctx := contextWithWorkspaceOwner(context.Background(), owner)
	payload := strings.Repeat("Write-Output stream\n", 512)
	if code, err := runSSHStreamResult(ctx, target, payload, io.Discard, io.Discard); err != nil || code != 0 {
		t.Fatalf("output stream code=%d err=%v", code, err)
	}
	streamInput := "streamed user input\n"
	if err := runSSHInputStream(ctx, target, payload, strings.NewReader(streamInput), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{1, 3} {
		stageCommand, stagedScript := readWorkspaceOwnerSSHCall(t, dir, index)
		if len(stageCommand) >= 8191 || !strings.Contains(stagedScript, base64.StdEncoding.EncodeToString([]byte(payload))) {
			t.Fatalf("stream stage %d command length=%d", index, len(stageCommand))
		}
	}
	_, noInput := readWorkspaceOwnerSSHCall(t, dir, 2)
	_, preservedInput := readWorkspaceOwnerSSHCall(t, dir, 4)
	if noInput != "" || preservedInput != streamInput {
		t.Fatalf("stream inputs output=%q input=%q", noInput, preservedInput)
	}
	count, err := os.ReadFile(filepath.Join(dir, "count"))
	if err != nil || string(count) != "4" {
		t.Fatalf("SSH call count=%q err=%v; staging must bypass owner recursion", count, err)
	}
}

func TestWorkspaceOwnerPOSIXTransportIsLoginShellIndependent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX transport execution requires POSIX shells")
	}

	shells := []struct {
		name string
		args []string
	}{
		{name: "bash", args: []string{"--noprofile", "--norc", "-c"}},
		{name: "zsh", args: []string{"-f", "-c"}},
		{name: "fish", args: []string{"--no-config", "-c"}},
	}
	for _, shell := range shells {
		t.Run(shell.name, func(t *testing.T) {
			shellPath, err := exec.LookPath(shell.name)
			if err != nil {
				t.Skipf("%s is not installed; portable launcher source remains covered", shell.name)
			}
			if shell.name == "bash" {
				if _, err := os.Stat("/bin/bash"); err == nil {
					shellPath = "/bin/bash"
				}
			}
			if shell.name == "zsh" {
				if _, err := os.Stat("/bin/zsh"); err == nil {
					shellPath = "/bin/zsh"
				}
			}

			const finalizeToken = "0123456789abcdef0123456789abcdef"
			home := filepath.Join(t.TempDir(), "owner's home")
			workdir := filepath.Join(home, "work root's checkout")
			metaDir := filepath.Join(workdir, ".crabbox")
			ownerRoot := filepath.Join(home, ".crabbox", "workspace-owners")
			if err := os.MkdirAll(metaDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(ownerRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(metaDir, remoteSyncPendingManifestName(finalizeToken)), []byte("tracked.txt\x00"), 0o644); err != nil {
				t.Fatal(err)
			}

			key := workspaceOwnerKey("cbx_login_shell_" + shell.name)
			token := strings.Repeat("a", 64)
			request := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: time.Minute}
			acquireTransport := remoteWorkspaceOwnerCommand(SSHTarget{TargetOS: targetLinux}, request)
			if !strings.HasPrefix(acquireTransport, "exec /bin/sh -c '") || strings.Contains(acquireTransport, "\n") || strings.Count(acquireTransport, "'") != 2 {
				t.Fatalf("workspace owner protocol is raw login-shell input: %q", acquireTransport[:min(len(acquireTransport), 80)])
			}
			acquireCmd := exec.Command(shellPath, append(shell.args, acquireTransport)...)
			acquireCmd.Env = append(os.Environ(), "HOME="+home)
			if out, err := acquireCmd.CombinedOutput(); err != nil || string(out) != "ACQUIRED" {
				t.Fatalf("owner acquisition failed through %s: out=%q err=%v", shell.name, out, err)
			}

			inputPath := filepath.Join(workdir, "preserved input.txt")
			payload := remoteFinalizeSync(workdir, remoteSyncFinalizeOptions{
				HydrateGit:  true,
				BaseRef:     "main",
				BaseSHA:     "abc123",
				Fingerprint: "fingerprint-with-a-'quote",
				Token:       finalizeToken,
				Coherence: gitCoherencePlan{
					RemoteURL: "https://example.test/owner's/repo.git",
					Target:    strings.Repeat("1", 40),
					Tree:      strings.Repeat("2", 40),
					Branch:    "main",
				},
			}) + " && cat > " + shellQuote(inputPath)
			transportCommand := remoteWorkspaceOwnerPOSIXWitness(key, token, payload, true)
			if !strings.HasPrefix(transportCommand, "exec /bin/sh -c '") || strings.Contains(transportCommand, "\n") || strings.Count(transportCommand, "'") != 2 {
				t.Fatalf("workspace owner transport is raw login-shell input: %q", transportCommand[:min(len(transportCommand), 80)])
			}
			if len(transportCommand) >= 30_000 {
				t.Fatalf("workspace owner transport is too large for a bounded Windows SSH command: %d bytes", len(transportCommand))
			}
			cmd := exec.Command(shellPath, append(shell.args, transportCommand)...)
			cmd.Env = append(os.Environ(), "HOME="+home)
			cmd.Stdin = strings.NewReader("preserved stdin\n")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("assembled transport failed through %s: %v\n%s", shell.name, err, out)
			}
			if got, err := os.ReadFile(filepath.Join(metaDir, "sync-manifest")); err != nil || string(got) != "tracked.txt\x00" {
				t.Fatalf("finalized manifest=%q err=%v", got, err)
			}
			if got, err := os.ReadFile(inputPath); err != nil || string(got) != "preserved stdin\n" {
				t.Fatalf("preserved input=%q err=%v", got, err)
			}
			entries, err := os.ReadDir(ownerRoot)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.Contains(entry.Name(), ".launcher.") || strings.Contains(entry.Name(), ".run.") || strings.HasSuffix(entry.Name(), ".child") {
					t.Fatalf("successful transport left temporary owner state %q", entry.Name())
				}
			}

			nonzero := remoteWorkspaceOwnerPOSIXWitness(key, token, "exit 23")
			nonzeroCmd := exec.Command(shellPath, append(shell.args, nonzero)...)
			nonzeroCmd.Env = append(os.Environ(), "HOME="+home)
			if out, err := nonzeroCmd.CombinedOutput(); err == nil || exitCode(err) != 23 {
				t.Fatalf("nonzero child through %s: out=%q err=%v", shell.name, out, err)
			}
		})
	}
}

func TestWorkspaceOwnerPOSIXLauncherRejectsMalformedPayload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX transport execution requires sh")
	}
	home := t.TempDir()
	key := workspaceOwnerKey("cbx_malformed_launcher")
	token := strings.Repeat("b", 64)
	marker := filepath.Join(home, "payload-ran")
	script := "touch " + shellQuote(marker)
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	transport := remoteWorkspaceOwnerPOSIXEncodedLauncher(key, token, encoded[:len(encoded)-1], len(script))
	cmd := exec.Command("sh", "-c", transport)
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err == nil || exitCode(err) != 74 {
		t.Fatalf("malformed launcher payload out=%q err=%v", out, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("malformed launcher executed payload: %v", err)
	}
	ownerRoot := filepath.Join(home, ".crabbox", "workspace-owners")
	entries, err := os.ReadDir(ownerRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".launcher.") {
			t.Fatalf("malformed launcher left private payload state %q", entry.Name())
		}
	}
}

func TestWorkspaceOwnerPOSIXLauncherDecoderFallbacks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX transport execution requires sh")
	}
	realBase64, err := exec.LookPath("base64")
	if err != nil {
		t.Skip("base64 is not installed")
	}
	realFlag := ""
	for _, flag := range []string{"--decode", "-d", "-D"} {
		cmd := exec.Command(realBase64, flag)
		cmd.Stdin = strings.NewReader("b2s=")
		if out, err := cmd.Output(); err == nil && string(out) == "ok" {
			realFlag = flag
			break
		}
	}
	if realFlag == "" {
		t.Skip("installed base64 has no supported decode flag")
	}

	tests := []struct {
		name          string
		acceptedFlag  string
		useOpenSSL    bool
		wantLogSuffix string
	}{
		{name: "long-gnu", acceptedFlag: "--decode", wantLogSuffix: "base64:--decode\n"},
		{name: "short-gnu", acceptedFlag: "-d", wantLogSuffix: "base64:--decode\nbase64:-d\n"},
		{name: "bsd", acceptedFlag: "-D", wantLogSuffix: "base64:--decode\nbase64:-d\nbase64:-D\n"},
		{name: "openssl", useOpenSSL: true, wantLogSuffix: "base64:--decode\nbase64:-d\nbase64:-D\nopenssl\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			tools := filepath.Join(home, "tools")
			if err := os.MkdirAll(tools, 0o755); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(home, "decoder.log")
			base64Script := "#!/bin/sh\nprintf 'base64:%s\\n' \"$1\" >> \"$CRABBOX_DECODER_LOG\"\n"
			if tt.useOpenSSL {
				base64Script += "exit 2\n"
			} else {
				base64Script += "[ \"$1\" = " + shellQuote(tt.acceptedFlag) + " ] || exit 2\nexec " + shellQuote(realBase64) + " " + shellQuote(realFlag) + "\n"
			}
			if err := os.WriteFile(filepath.Join(tools, "base64"), []byte(base64Script), 0o755); err != nil {
				t.Fatal(err)
			}
			opensslScript := "#!/bin/sh\nprintf 'openssl\\n' >> \"$CRABBOX_DECODER_LOG\"\n"
			if tt.useOpenSSL {
				opensslScript += "[ \"$1\" = base64 ] || exit 2\nexec " + shellQuote(realBase64) + " " + shellQuote(realFlag) + "\n"
			} else {
				opensslScript += "exit 2\n"
			}
			if err := os.WriteFile(filepath.Join(tools, "openssl"), []byte(opensslScript), 0o755); err != nil {
				t.Fatal(err)
			}

			marker := filepath.Join(home, "decoded")
			transport := remoteWorkspaceOwnerPOSIXLauncher(strings.Repeat("c", 64), strings.Repeat("d", 64), "printf decoder-ok > "+shellQuote(marker))
			cmd := exec.Command("sh", "-c", transport)
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"), "CRABBOX_DECODER_LOG="+logPath)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("decoder fallback failed: %v\n%s", err, out)
			}
			if got, err := os.ReadFile(marker); err != nil || string(got) != "decoder-ok" {
				t.Fatalf("decoded payload=%q err=%v", got, err)
			}
			if got, err := os.ReadFile(logPath); err != nil || string(got) != tt.wantLogSuffix {
				t.Fatalf("decoder attempts=%q want %q err=%v", got, tt.wantLogSuffix, err)
			}
		})
	}
}

func runPOSIXWorkspaceOwnerScript(t *testing.T, home, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

type posixChildRecord struct {
	pgid          int
	sentinelPID   int
	startIdentity string
}

func readPOSIXChildRecord(t *testing.T, path string) posixChildRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) == 4 && lines[0] == "v2" {
				pgid, pgidErr := strconv.Atoi(lines[1])
				sentinelPID, pidErr := strconv.Atoi(lines[2])
				if pgidErr == nil && pidErr == nil && pgid > 1 && sentinelPID > 1 && lines[3] != "" {
					return posixChildRecord{pgid: pgid, sentinelPID: sentinelPID, startIdentity: lines[3]}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("POSIX child record was not published at %s: %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func processIsLive(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.TrimSpace(string(out)) != "" && !strings.Contains(strings.TrimSpace(string(out)), "Z")
}

func killPOSIXProcessGroup(pgid int) error {
	if pgid <= 1 {
		return fmt.Errorf("refusing to signal process group %d", pgid)
	}
	return exec.Command("kill", "-KILL", "--", "-"+strconv.Itoa(pgid)).Run()
}

func killPOSIXGroupIfProcessMatches(pgid, processPID int, startIdentity string) {
	// Cleanup must re-fence a live member; the runner may reuse a dead group's PGID.
	if pgid <= 1 || processPID <= 1 || startIdentity == "" {
		return
	}
	pid := strconv.Itoa(processPID)
	pgidOutput, pgidErr := exec.Command("ps", "-o", "pgid=", "-p", pid).Output()
	stat, statErr := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
	identity, identityErr := exec.Command("ps", "-o", "lstart=", "-p", pid).Output()
	livePGID, _ := strconv.Atoi(strings.TrimSpace(string(pgidOutput)))
	liveIdentity := strings.TrimSpace(strings.Join(strings.Fields(string(identity)), " "))
	if pgidErr != nil || statErr != nil || identityErr != nil ||
		livePGID != pgid || strings.Contains(string(stat), "Z") ||
		liveIdentity != startIdentity {
		return
	}
	_ = killPOSIXProcessGroup(pgid)
}

func killPOSIXGroupIfWitnessMatches(record posixChildRecord) {
	killPOSIXGroupIfProcessMatches(record.pgid, record.sentinelPID, record.startIdentity)
}

func posixToolsWithoutSessionHelpers(t *testing.T, home string) string {
	t.Helper()
	tools := filepath.Join(home, "tools-no-session-helpers")
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	names := []string{"awk", "base64", "chmod", "cut", "date", "mkdir", "mkfifo", "mv", "nohup", "ps", "rm", "rmdir", "sed", "sh", "sleep", "touch", "tr", "wc"}
	lockFound := false
	for _, name := range append(names, "flock", "lockf") {
		source, err := exec.LookPath(name)
		if err != nil {
			if name == "flock" || name == "lockf" {
				continue
			}
			t.Skipf("required POSIX test tool %s is unavailable", name)
		}
		if name == "flock" || name == "lockf" {
			lockFound = true
		}
		if err := os.Symlink(source, filepath.Join(tools, name)); err != nil {
			t.Fatal(err)
		}
	}
	if !lockFound {
		t.Skip("flock or lockf is required")
	}
	return tools
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for processIsLive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d remained live", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorkspaceOwnerPOSIXProtocolBehavior(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX protocol execution requires sh")
	}
	home := t.TempDir()
	key := workspaceOwnerKey("cbx_protocol")
	tokenA := strings.Repeat("a", 64)
	tokenB := strings.Repeat("b", 64)
	request := func(action workspaceOwnerAction, token string) workspaceOwnerRemoteRequest {
		return workspaceOwnerRemoteRequest{Action: action, Key: key, Token: token, TTL: 30 * time.Second}
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenA))); err != nil || out != "ACQUIRED" {
		t.Fatalf("acquire out=%q err=%v", out, err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenA))); err != nil || out != "ACQUIRED" {
		t.Fatalf("same-token acquire replay out=%q err=%v", out, err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerRenew, tokenB))); err == nil || out != "MISMATCH" {
		t.Fatalf("mismatched renew out=%q err=%v", out, err)
	}
	statePath := filepath.Join(home, ".crabbox", "workspace-owners", key+".owner")
	childPath := filepath.Join(home, ".crabbox", "workspace-owners", key+".child")
	expiredState := "v1\n" + tokenA + "\n1\n"
	if err := os.WriteFile(statePath, []byte(expiredState), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerRenew, tokenA))); err == nil || out != "EXPIRED" {
		t.Fatalf("late same-token renew out=%q err=%v", out, err)
	}
	if data, err := os.ReadFile(statePath); err != nil || string(data) != expiredState {
		t.Fatalf("late renew changed expired state: data=%q err=%v", data, err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerInspect, tokenA))); err == nil || out != "EXPIRED" {
		t.Fatalf("late same-token inspect out=%q err=%v", out, err)
	}
	lateResultPath := filepath.Join(home, "late-child.txt")
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIXWitness(key, tokenA, "touch "+shellQuote(lateResultPath))); err == nil {
		t.Fatalf("late same-token witness succeeded: out=%q", out)
	}
	if _, err := os.Stat(childPath); !os.IsNotExist(err) {
		t.Fatalf("late witness published child state: %v", err)
	}
	if _, err := os.Stat(lateResultPath); !os.IsNotExist(err) {
		t.Fatalf("late witness executed child command: %v", err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenA))); err != nil || out != "RECOVERED" {
		t.Fatalf("recover expired generation out=%q err=%v", out, err)
	}

	resultPath := filepath.Join(home, "revision.txt")
	witness := remoteWorkspaceOwnerPOSIXWitness(key, tokenA, "printf revision-a > "+shellQuote(resultPath))
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, witness); err != nil {
		t.Fatalf("witnessed command out=%q err=%v", out, err)
	}
	if data, err := os.ReadFile(resultPath); err != nil || string(data) != "revision-a" {
		t.Fatalf("witnessed result=%q err=%v", data, err)
	}
	inputPath := filepath.Join(home, "input.txt")
	inputWitness := remoteWorkspaceOwnerPOSIXWitness(key, tokenA, "cat > "+shellQuote(inputPath), true)
	cmd := exec.Command("sh", "-c", inputWitness)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = strings.NewReader("registration-input")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("input witness out=%q err=%v", out, err)
	}
	if data, err := os.ReadFile(inputPath); err != nil || string(data) != "registration-input" {
		t.Fatalf("witnessed input=%q err=%v", data, err)
	}
	const childExitCode = 42
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIXWitness(key, tokenA, "exit "+strconv.Itoa(childExitCode))); err == nil {
		t.Fatalf("nonzero witnessed command succeeded: out=%q", out)
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != childExitCode {
		t.Fatalf("nonzero witnessed command out=%q err=%v, want exit code %d", out, err, childExitCode)
	}
	if _, err := os.Stat(childPath); !os.IsNotExist(err) {
		t.Fatalf("nonzero witnessed command left child state: %v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(home, ".crabbox", "workspace-owners", key+".run."+tokenA+".*")); err != nil || len(matches) != 0 {
		t.Fatalf("nonzero witnessed command left run state %q: %v", matches, err)
	}
	laterResultPath := filepath.Join(home, "after-nonzero.txt")
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIXWitness(key, tokenA, "touch "+shellQuote(laterResultPath))); err != nil {
		t.Fatalf("witness after nonzero child out=%q err=%v", out, err)
	}
	if _, err := os.Stat(laterResultPath); err != nil {
		t.Fatalf("command after nonzero child did not proceed: %v", err)
	}
	identityOut, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Fatal(err)
	}
	identity := strings.TrimSpace(strings.Join(strings.Fields(string(identityOut)), " "))
	existingWitness := strconv.Itoa(os.Getpid()) + "\n" + identity + "\n"
	if err := os.WriteFile(childPath, []byte(existingWitness), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIXWitness(key, tokenA, "true")); err == nil {
		t.Fatalf("overlapping witness replaced live child: out=%q", out)
	}
	if data, err := os.ReadFile(childPath); err != nil || string(data) != existingWitness {
		t.Fatalf("live witness changed: data=%q err=%v", data, err)
	}
	if err := os.Remove(childPath); err != nil {
		t.Fatal(err)
	}
	guardOwner := &workspaceOwner{target: SSHTarget{TargetOS: targetLinux}, key: key, token: tokenA}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, guardOwner.wrapPOSIXBackgroundCommand(guardOwner.rsyncGuardPayload(filepath.Join(home, "guard-destination")))); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("start rsync guard out=%q err=%v", out, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(childPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rsync guard did not publish its child witness")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, guardOwner.rsyncStopCommand()); err != nil {
		t.Fatalf("stop rsync guard out=%q err=%v", out, err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(childPath); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rsync guard did not clear its child witness")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(childPath); !os.IsNotExist(err) {
		t.Fatalf("child witness remained after exit: %v", err)
	}

	if err := os.WriteFile(statePath, []byte("v1\n"+tokenA+"\n1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyLive := strconv.Itoa(os.Getpid()) + "\n" + identity + "\n"
	if err := os.WriteFile(statePath, []byte(fmt.Sprintf("v1\n%s\n%d\n", tokenA, time.Now().Add(30*time.Second).Unix())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, []byte(legacyLive), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, action := range []workspaceOwnerAction{workspaceOwnerInspect, workspaceOwnerRelease} {
		if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(action, tokenA))); err != nil || out != "LEGACY" {
			t.Fatalf("unexpired legacy %s out=%q err=%v", action, out, err)
		}
		if data, err := os.ReadFile(childPath); err != nil || string(data) != legacyLive {
			t.Fatalf("unexpired legacy %s changed record: data=%q err=%v", action, data, err)
		}
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenB))); err != nil || out != "BUSY" {
		t.Fatalf("unexpired legacy owner acquire out=%q err=%v", out, err)
	}
	if data, err := os.ReadFile(childPath); err != nil || string(data) != legacyLive {
		t.Fatalf("unexpired legacy record changed: data=%q err=%v", data, err)
	}
	if err := os.WriteFile(statePath, []byte("v1\n"+tokenA+"\n1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyRecords := []struct {
		name     string
		contents string
	}{
		{name: "live", contents: legacyLive},
		{name: "dead", contents: "999999999\ndead identity\n"},
		{name: "identity mismatch", contents: strconv.Itoa(os.Getpid()) + "\nmismatched identity\n"},
	}
	for _, test := range legacyRecords {
		t.Run("legacy "+test.name, func(t *testing.T) {
			if err := os.WriteFile(childPath, []byte(test.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenB))); err != nil || out != "LEGACY" {
				t.Fatalf("legacy acquire out=%q err=%v", out, err)
			}
			if data, err := os.ReadFile(childPath); err != nil || string(data) != test.contents {
				t.Fatalf("legacy record changed: data=%q err=%v", data, err)
			}
		})
	}
	malformedLegacy := "not-a-pid\nidentity\n"
	if err := os.WriteFile(childPath, []byte(malformedLegacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenB))); err == nil || out != "AMBIGUOUS" {
		t.Fatalf("malformed legacy acquire out=%q err=%v", out, err)
	}
	if data, err := os.ReadFile(childPath); err != nil || string(data) != malformedLegacy {
		t.Fatalf("malformed legacy record changed: data=%q err=%v", data, err)
	}
	if err := os.WriteFile(childPath, []byte("v2\n999999998\n999999999\nold identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenB))); err != nil || out != "RECOVERED" {
		t.Fatalf("stale recovery out=%q err=%v", out, err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerRelease, tokenB))); err != nil || out != "RELEASED" {
		t.Fatalf("release out=%q err=%v", out, err)
	}
}

func TestWorkspaceOwnerPOSIXSupervisorSurvivesTransportLoss(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX supervisor execution requires sh")
	}
	home := t.TempDir()
	key := workspaceOwnerKey("cbx_supervisor_transport_loss")
	tokenA := strings.Repeat("a", 64)
	tokenB := strings.Repeat("b", 64)
	request := func(action workspaceOwnerAction, token string) workspaceOwnerRemoteRequest {
		return workspaceOwnerRemoteRequest{Action: action, Key: key, Token: token, TTL: 30 * time.Second}
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenA))); err != nil || out != "ACQUIRED" {
		t.Fatalf("acquire out=%q err=%v", out, err)
	}
	tools := filepath.Join(home, "tools")
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	realPS, err := exec.LookPath("ps")
	if err != nil {
		t.Fatal(err)
	}
	failPS := filepath.Join(home, "fail-supervisor-ps")
	psShim := `#!/bin/sh
if [ -f "$CRABBOX_FAIL_PS" ] && [ "$1:$2" = "-o:stat=" ]; then exit 1; fi
exec ` + shellQuote(realPS) + ` "$@"
`
	if err := os.WriteFile(filepath.Join(tools, "ps"), []byte(psShim), 0o700); err != nil {
		t.Fatal(err)
	}

	sentinel := exec.Command("sleep", "30")
	if err := sentinel.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sentinel.Process.Kill()
		_ = sentinel.Wait()
	})

	pidsPath := filepath.Join(home, "workload-pids")
	payload := `sleep 30 & descendant=$!; printf '%s\n%s\n' "$$" "$descendant" > ` + shellQuote(pidsPath) + `; wait`
	witnessCommand := []string{"sh", "-c", remoteWorkspaceOwnerPOSIXWitness(key, tokenA, payload)}
	killWitness := func(cmd *exec.Cmd) error { return cmd.Process.Kill() }
	if setsid, err := exec.LookPath("setsid"); err == nil {
		witnessCommand = append([]string{setsid}, witnessCommand...)
		killWitness = func(cmd *exec.Cmd) error {
			return killPOSIXProcessGroup(cmd.Process.Pid)
		}
	}
	witness := exec.Command(witnessCommand[0], witnessCommand[1:]...)
	witness.Env = append(os.Environ(), "HOME="+home, "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"), "CRABBOX_FAIL_PS="+failPS)
	if err := witness.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = witness.Process.Kill()
		_ = witness.Wait()
	})

	var workloadPID, descendantPID int
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(pidsPath)
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) == 2 {
				workloadPID, _ = strconv.Atoi(fields[0])
				descendantPID, _ = strconv.Atoi(fields[1])
				if workloadPID > 0 && descendantPID > 0 {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("workload did not publish descendant PIDs: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := killWitness(witness); err != nil {
		t.Fatal(err)
	}
	_ = witness.Wait()
	if err := os.WriteFile(failPS, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{workloadPID, descendantPID} {
		deadline := time.Now().Add(5 * time.Second)
		for exec.Command("kill", "-0", strconv.Itoa(pid)).Run() == nil {
			if time.Now().After(deadline) {
				t.Fatalf("fenced workload process %d survived supervisor termination", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	statePath := filepath.Join(home, ".crabbox", "workspace-owners", key+".owner")
	if err := os.WriteFile(statePath, []byte("v1\n"+tokenA+"\n1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("kill", "-0", strconv.Itoa(sentinel.Process.Pid)).Run(); err != nil {
		t.Fatalf("unrelated sentinel was terminated: %v", err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for {
		out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenB)))
		if err == nil && out == "RECOVERED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reacquire after supervisor cleanup out=%q err=%v", out, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerRelease, tokenB))); err != nil || out != "RELEASED" {
		t.Fatalf("release after supervisor cleanup out=%q err=%v", out, err)
	}
}

func TestWorkspaceOwnerPOSIXOverlappingWitnessKeepsActiveSupervisor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX supervisor execution requires sh")
	}
	home := t.TempDir()
	key := workspaceOwnerKey("cbx_overlapping_witness")
	token := strings.Repeat("a", 64)
	request := func(action workspaceOwnerAction) workspaceOwnerRemoteRequest {
		return workspaceOwnerRemoteRequest{Action: action, Key: key, Token: token, TTL: 30 * time.Second}
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire))); err != nil || out != "ACQUIRED" {
		t.Fatalf("acquire out=%q err=%v", out, err)
	}
	activePath := filepath.Join(home, "active-witness")
	rejectedPath := filepath.Join(home, "rejected-witness")
	first := exec.Command("sh", "-c", remoteWorkspaceOwnerPOSIXWitness(key, token, "touch "+shellQuote(activePath)+"; sleep 30"))
	first.Env = append(os.Environ(), "HOME="+home)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(home, ".crabbox", "workspace-owners", key+".child")
	record := readPOSIXChildRecord(t, childPath)
	t.Cleanup(func() {
		killPOSIXGroupIfWitnessMatches(record)
		_ = first.Process.Kill()
		_ = first.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(activePath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("active witness payload did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIXWitness(key, token, "touch "+shellQuote(rejectedPath))); err == nil || exitCode(err) != 74 {
		t.Fatalf("overlapping witness out=%q err=%v", out, err)
	}
	if _, err := os.Stat(rejectedPath); !os.IsNotExist(err) {
		t.Fatalf("overlapping witness executed: %v", err)
	}
	after := readPOSIXChildRecord(t, childPath)
	if after != record || !processIsLive(record.pgid) || !processIsLive(record.sentinelPID) {
		t.Fatalf("overlapping witness disturbed active supervisor: before=%+v after=%+v", record, after)
	}
	statePath := filepath.Join(home, ".crabbox", "workspace-owners", key+".owner")
	if err := os.WriteFile(statePath, []byte("v1\n"+token+"\n1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err == nil {
		t.Fatal("expired active witness succeeded")
	}
	waitForProcessExit(t, record.sentinelPID)
}

func TestWorkspaceOwnerPOSIXSentinelRetainsAndExpiresLeaderlessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX supervisor execution requires sh")
	}
	home := t.TempDir()
	key := workspaceOwnerKey("cbx_sentinel_leader_exit")
	tokenA := strings.Repeat("a", 64)
	tokenB := strings.Repeat("b", 64)
	request := func(action workspaceOwnerAction, token string) workspaceOwnerRemoteRequest {
		return workspaceOwnerRemoteRequest{Action: action, Key: key, Token: token, TTL: 30 * time.Second}
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenA))); err != nil || out != "ACQUIRED" {
		t.Fatalf("acquire out=%q err=%v", out, err)
	}

	unrelated := exec.Command("sleep", "30")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unrelated.Process.Kill()
		_ = unrelated.Wait()
	})

	pidsPath := filepath.Join(home, "leader-exit-pids")
	payload := `sleep 30 </dev/null >/dev/null 2>&1 & descendant=$!; printf '%s\n%s\n' "$$" "$descendant" > ` + shellQuote(pidsPath) + `; wait`
	var witnessOutput bytes.Buffer
	witness := exec.Command("sh", "-c", remoteWorkspaceOwnerPOSIXWitness(key, tokenA, payload))
	witness.Env = append(os.Environ(), "HOME="+home)
	witness.Stdout = &witnessOutput
	witness.Stderr = &witnessOutput
	if err := witness.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = witness.Process.Kill()
		_ = witness.Wait()
	})

	var data []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ = os.ReadFile(pidsPath)
		if len(strings.Fields(string(data))) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("leader exit workload did not publish PIDs: %q", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	fields := strings.Fields(string(data))
	workloadPID, _ := strconv.Atoi(fields[0])
	descendantPID, _ := strconv.Atoi(fields[1])
	childPath := filepath.Join(home, ".crabbox", "workspace-owners", key+".child")
	record := readPOSIXChildRecord(t, childPath)
	t.Cleanup(func() { killPOSIXGroupIfWitnessMatches(record) })
	if err := exec.Command("kill", "-KILL", strconv.Itoa(record.pgid)).Run(); err != nil {
		t.Fatal(err)
	}
	waitForProcessExit(t, record.pgid)
	for _, pid := range []int{record.sentinelPID, workloadPID, descendantPID} {
		if !processIsLive(pid) {
			t.Fatalf("leader exit unexpectedly terminated process %d", pid)
		}
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerInspect, tokenA))); err != nil || out != "CHILD" {
		t.Fatalf("leaderless child inspect out=%q err=%v", out, err)
	}

	statePath := filepath.Join(home, ".crabbox", "workspace-owners", key+".owner")
	if err := os.WriteFile(statePath, []byte("v1\n"+tokenA+"\n1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := witness.Wait(); err == nil {
		t.Fatal("expired leader-exit witness succeeded")
	} else if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("leader-exit witness err=%v output=%q", err, witnessOutput.String())
	}
	for _, pid := range []int{record.sentinelPID, workloadPID, descendantPID} {
		waitForProcessExit(t, pid)
	}
	if err := exec.Command("kill", "-0", strconv.Itoa(unrelated.Process.Pid)).Run(); err != nil {
		t.Fatalf("unrelated sentinel was terminated: %v", err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenB))); err != nil || out != "RECOVERED" {
		t.Fatalf("reacquire after leader-exit cleanup out=%q err=%v", out, err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerRelease, tokenB))); err != nil || out != "RELEASED" {
		t.Fatalf("release after leader-exit cleanup out=%q err=%v", out, err)
	}
}

func TestWorkspaceOwnerPOSIXSentinelDeathFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX supervisor execution requires sh")
	}
	home := t.TempDir()
	key := workspaceOwnerKey("cbx_sentinel_death")
	tokenA := strings.Repeat("a", 64)
	tokenB := strings.Repeat("b", 64)
	request := func(action workspaceOwnerAction, token string) workspaceOwnerRemoteRequest {
		return workspaceOwnerRemoteRequest{Action: action, Key: key, Token: token, TTL: 30 * time.Second}
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenA))); err != nil || out != "ACQUIRED" {
		t.Fatalf("acquire out=%q err=%v", out, err)
	}
	pidsPath := filepath.Join(home, "sentinel-death-pids")
	payload := `sleep 30 & descendant=$!; printf '%s\n%s\n' "$$" "$descendant" > ` + shellQuote(pidsPath) + `; wait`
	witness := exec.Command("sh", "-c", remoteWorkspaceOwnerPOSIXWitness(key, tokenA, payload))
	witness.Env = append(os.Environ(), "HOME="+home)
	if err := witness.Start(); err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(home, ".crabbox", "workspace-owners", key+".child")
	record := readPOSIXChildRecord(t, childPath)
	leaderIdentity, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(record.pgid)).Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killPOSIXGroupIfProcessMatches(record.pgid, record.pgid, strings.TrimSpace(strings.Join(strings.Fields(string(leaderIdentity)), " ")))
		_ = witness.Wait()
	})
	var descendantPID int
	deadline := time.Now().Add(5 * time.Second)
	for descendantPID == 0 {
		data, _ := os.ReadFile(pidsPath)
		fields := strings.Fields(string(data))
		if len(fields) == 2 {
			descendantPID, _ = strconv.Atoi(fields[1])
		}
		if time.Now().After(deadline) {
			t.Fatal("sentinel-death payload did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := exec.Command("kill", "-KILL", strconv.Itoa(record.sentinelPID)).Run(); err != nil {
		t.Fatal(err)
	}
	waitForProcessExit(t, record.sentinelPID)
	statePath := filepath.Join(home, ".crabbox", "workspace-owners", key+".owner")
	if err := os.WriteFile(statePath, []byte("v1\n"+tokenA+"\n1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenB))); err == nil || out != "AMBIGUOUS" {
		t.Fatalf("sentinel-death acquire out=%q err=%v", out, err)
	}
	time.Sleep(300 * time.Millisecond)
	for _, pid := range []int{record.pgid, descendantPID} {
		if !processIsLive(pid) {
			t.Fatalf("invalid sentinel caused group signal to process %d", pid)
		}
	}
}

func TestWorkspaceOwnerPOSIXAmbientTransportGroupFallbackFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX supervisor execution requires sh")
	}
	home := t.TempDir()
	tools := posixToolsWithoutSessionHelpers(t, home)
	key := workspaceOwnerKey("cbx_ambient_transport_group")
	token := strings.Repeat("a", 64)
	request := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: 30 * time.Second}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request)); err != nil || out != "ACQUIRED" {
		t.Fatalf("acquire out=%q err=%v", out, err)
	}
	marker := filepath.Join(home, "ambient-payload")
	cmd := exec.Command(filepath.Join(tools, "sh"), "-c", remoteWorkspaceOwnerPOSIXWitness(key, token, "touch "+shellQuote(marker)))
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+tools)
	if out, err := cmd.CombinedOutput(); err == nil || exitCode(err) != 74 {
		t.Fatalf("ambient fallback out=%q err=%v", out, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("ambient fallback executed payload: %v", err)
	}
	childPath := filepath.Join(home, ".crabbox", "workspace-owners", key+".child")
	if _, err := os.Stat(childPath); !os.IsNotExist(err) {
		t.Fatalf("ambient fallback published child state: %v", err)
	}
}

func TestWorkspaceOwnerPOSIXIsolatedTransportGroupFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX supervisor execution requires sh")
	}
	home := t.TempDir()
	tools := posixToolsWithoutSessionHelpers(t, home)
	sshMode := os.Getenv("CRABBOX_TEST_SSH_LOCALHOST") == "1"
	runWithTools := func(script string) (string, error) {
		var cmd *exec.Cmd
		if sshMode {
			remote := "export HOME=" + shellQuote(home) + "; export PATH=" + shellQuote(tools) + "; " + script
			cmd = exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "localhost", remote)
		} else {
			cmd = exec.Command(filepath.Join(tools, "sh"), "-c", script)
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+tools)
		}
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := runWithTools("command -v setsid || command -v perl"); err == nil {
		t.Fatalf("session helper unexpectedly available: %q", out)
	}

	key := workspaceOwnerKey("cbx_isolated_transport_group")
	tokenA := strings.Repeat("a", 64)
	tokenB := strings.Repeat("b", 64)
	request := func(action workspaceOwnerAction, token string) workspaceOwnerRemoteRequest {
		return workspaceOwnerRemoteRequest{Action: action, Key: key, Token: token, TTL: 30 * time.Second}
	}
	if out, err := runWithTools(remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenA))); err != nil || out != "ACQUIRED" {
		t.Fatalf("acquire out=%q err=%v", out, err)
	}

	unrelated := exec.Command("sleep", "30")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unrelated.Process.Kill()
		_ = unrelated.Wait()
	})

	pidsPath := filepath.Join(home, "fallback-pids")
	payload := `sleep 30 & descendant=$!; printf '%s\n%s\n' "$$" "$descendant" > ` + shellQuote(pidsPath) + `; wait`
	transport := remoteWorkspaceOwnerPOSIXWitness(key, tokenA, payload)
	var command []string
	if sshMode {
		remote := "export HOME=" + shellQuote(home) + "; export PATH=" + shellQuote(tools) + "; " + transport
		command = []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "localhost", remote}
	} else if setsid, err := exec.LookPath("setsid"); err == nil {
		command = []string{setsid, "/bin/sh", "-c", transport}
	} else if perl, err := exec.LookPath("perl"); err == nil {
		command = []string{perl, "-MPOSIX", "-e", "POSIX::setsid() >= 0 or exit 74; exec @ARGV", "/bin/sh", "-c", transport}
	} else {
		t.Skip("test transport cannot create an isolated process group")
	}
	var witnessOutput bytes.Buffer
	witness := exec.Command(command[0], command[1:]...)
	witness.Env = append(os.Environ(), "HOME="+home, "PATH="+tools)
	witness.Stdout = &witnessOutput
	witness.Stderr = &witnessOutput
	if err := witness.Start(); err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(home, ".crabbox", "workspace-owners", key+".child")
	record := readPOSIXChildRecord(t, childPath)
	t.Cleanup(func() {
		killPOSIXGroupIfWitnessMatches(record)
		_ = witness.Process.Kill()
		_ = witness.Wait()
	})
	if !sshMode && record.pgid != witness.Process.Pid {
		t.Fatalf("fallback group=%d transport=%d", record.pgid, witness.Process.Pid)
	}

	var workloadPID, descendantPID int
	deadline := time.Now().Add(5 * time.Second)
	for workloadPID == 0 || descendantPID == 0 {
		data, _ := os.ReadFile(pidsPath)
		fields := strings.Fields(string(data))
		if len(fields) == 2 {
			workloadPID, _ = strconv.Atoi(fields[0])
			descendantPID, _ = strconv.Atoi(fields[1])
		}
		if time.Now().After(deadline) {
			t.Fatalf("fallback payload did not publish PIDs: %q", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	statePath := filepath.Join(home, ".crabbox", "workspace-owners", key+".owner")
	if err := os.WriteFile(statePath, []byte("v1\n"+tokenA+"\n1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := witness.Wait(); err == nil {
		t.Fatalf("expired fallback witness succeeded: %q", witnessOutput.String())
	}
	for _, pid := range []int{record.sentinelPID, workloadPID, descendantPID} {
		waitForProcessExit(t, pid)
	}
	if err := exec.Command("kill", "-0", strconv.Itoa(unrelated.Process.Pid)).Run(); err != nil {
		t.Fatalf("unrelated sentinel was terminated: %v", err)
	}
	if out, err := runWithTools(remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenB))); err != nil || out != "RECOVERED" {
		t.Fatalf("fallback reacquire out=%q err=%v", out, err)
	}
	if matches, err := filepath.Glob(filepath.Join(home, ".crabbox", "workspace-owners", key+".launcher."+tokenA+".*")); err != nil || len(matches) != 0 {
		t.Fatalf("fallback recovery left launcher state %q: %v", matches, err)
	}
	if out, err := runWithTools(remoteWorkspaceOwnerPOSIX(request(workspaceOwnerRelease, tokenB))); err != nil || out != "RELEASED" {
		t.Fatalf("fallback release out=%q err=%v", out, err)
	}
}

func TestWorkspaceOwnerPOSIXTamperedSentinelRecordsFailClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX protocol execution requires sh")
	}
	home := t.TempDir()
	key := workspaceOwnerKey("cbx_sentinel_tamper")
	tokenA := strings.Repeat("a", 64)
	request := func(action workspaceOwnerAction, token string) workspaceOwnerRemoteRequest {
		return workspaceOwnerRemoteRequest{Action: action, Key: key, Token: token, TTL: 30 * time.Second}
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenA))); err != nil || out != "ACQUIRED" {
		t.Fatalf("acquire out=%q err=%v", out, err)
	}
	startedPath := filepath.Join(home, "tamper-started")
	witness := exec.Command("sh", "-c", remoteWorkspaceOwnerPOSIXWitness(key, tokenA, "touch "+shellQuote(startedPath)+"; sleep 30"))
	witness.Env = append(os.Environ(), "HOME="+home)
	if err := witness.Start(); err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(home, ".crabbox", "workspace-owners", key+".child")
	record := readPOSIXChildRecord(t, childPath)
	t.Cleanup(func() {
		killPOSIXGroupIfWitnessMatches(record)
		_ = witness.Process.Kill()
		_ = witness.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tamper workload did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	records := []struct {
		name     string
		contents string
	}{
		{name: "identity", contents: fmt.Sprintf("v2\n%d\n%d\ntampered identity\n", record.pgid, record.sentinelPID)},
		{name: "pgid", contents: fmt.Sprintf("v2\n%d\n%d\n%s\n", record.pgid+100000000, record.sentinelPID, record.startIdentity)},
		{name: "id one", contents: "v2\n1\n1\nidentity\n"},
	}
	for _, tt := range records {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(childPath, []byte(tt.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerInspect, tokenA))); err == nil || out != "AMBIGUOUS" {
				t.Fatalf("tampered inspect out=%q err=%v", out, err)
			}
			if !processIsLive(record.pgid) || !processIsLive(record.sentinelPID) {
				t.Fatal("tampered record signaled the live workload group")
			}
		})
	}
	if err := killPOSIXProcessGroup(record.pgid); err != nil {
		t.Fatal(err)
	}
	_ = witness.Process.Kill()
	_ = witness.Wait()

	zombie := exec.Command("sh", "-c", "exit 0")
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	defer zombie.Wait()
	deadline = time.Now().Add(2 * time.Second)
	var zombiePGID int
	var zombieIdentity string
	for {
		stat, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(zombie.Process.Pid)).Output()
		pgid, _ := exec.Command("ps", "-o", "pgid=", "-p", strconv.Itoa(zombie.Process.Pid)).Output()
		identity, _ := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(zombie.Process.Pid)).Output()
		zombiePGID, _ = strconv.Atoi(strings.TrimSpace(string(pgid)))
		zombieIdentity = strings.TrimSpace(strings.Join(strings.Fields(string(identity)), " "))
		if strings.Contains(string(stat), "Z") && zombiePGID > 1 && zombieIdentity != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed to retain zombie child for protocol test")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(childPath, []byte(fmt.Sprintf("v2\n%d\n%d\n%s\n", zombiePGID, zombie.Process.Pid, zombieIdentity)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerInspect, tokenA))); err == nil || out != "AMBIGUOUS" {
		t.Fatalf("zombie sentinel inspect out=%q err=%v", out, err)
	}
}

func TestWorkspaceOwnerPOSIXNormalCompletionReapsSentinel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX supervisor execution requires sh")
	}
	home := t.TempDir()
	key := workspaceOwnerKey("cbx_sentinel_normal")
	token := strings.Repeat("a", 64)
	request := func(action workspaceOwnerAction) workspaceOwnerRemoteRequest {
		return workspaceOwnerRemoteRequest{Action: action, Key: key, Token: token, TTL: 30 * time.Second}
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire))); err != nil || out != "ACQUIRED" {
		t.Fatalf("acquire out=%q err=%v", out, err)
	}
	root := filepath.Join(home, ".crabbox", "workspace-owners")
	childPath := filepath.Join(root, key+".child")
	tools := filepath.Join(home, "tools")
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	realPS, err := exec.LookPath("ps")
	if err != nil {
		t.Fatal(err)
	}
	psShim := `#!/bin/sh
` + shellQuote(realPS) + ` "$@"
status=$?
if [ "$1:$2" = "-eo:pid=,pgid=,stat=" ] && [ -f "$CRABBOX_TEST_CHILD_RECORD" ]; then
	pgid=$(sed -n '2p' "$CRABBOX_TEST_CHILD_RECORD")
	printf '999999 %s Z\n' "$pgid"
fi
exit "$status"
`
	if err := os.WriteFile(filepath.Join(tools, "ps"), []byte(psShim), 0o700); err != nil {
		t.Fatal(err)
	}
	publishedPath := filepath.Join(home, "published-child")
	descendantPath := filepath.Join(home, "normal-descendant")
	payload := `test -f ` + shellQuote(childPath) + ` || exit 70; cp ` + shellQuote(childPath) + ` ` + shellQuote(publishedPath) + `; sleep 0.3 & printf '%s\n' "$!" > ` + shellQuote(descendantPath) + `; exit 42`
	witness := exec.Command("sh", "-c", remoteWorkspaceOwnerPOSIXWitness(key, token, payload))
	witness.Env = append(os.Environ(), "HOME="+home, "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"), "CRABBOX_TEST_CHILD_RECORD="+childPath)
	started := time.Now()
	if err := witness.Start(); err != nil {
		t.Fatal(err)
	}
	record := readPOSIXChildRecord(t, childPath)
	err = witness.Wait()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 42 {
		t.Fatalf("normal witness err=%v, want exit 42", err)
	}
	if time.Since(started) < 250*time.Millisecond {
		t.Fatal("normal completion did not wait for its in-group descendant")
	}
	published, err := os.ReadFile(publishedPath)
	if err != nil || !strings.HasPrefix(string(published), "v2\n") {
		t.Fatalf("payload started before v2 record publication: data=%q err=%v", published, err)
	}
	descendantData, err := os.ReadFile(descendantPath)
	if err != nil {
		t.Fatal(err)
	}
	descendantPID, _ := strconv.Atoi(strings.TrimSpace(string(descendantData)))
	waitForProcessExit(t, descendantPID)
	waitForProcessExit(t, record.sentinelPID)
	if _, err := os.Stat(childPath); !os.IsNotExist(err) {
		t.Fatalf("normal completion left child state: %v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(root, key+".run."+token+".*")); err != nil || len(matches) != 0 {
		t.Fatalf("normal completion left run state %q: %v", matches, err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerRelease))); err != nil || out != "RELEASED" {
		t.Fatalf("release after normal completion out=%q err=%v", out, err)
	}
}

func TestWorkspaceOwnerPOSIXRecoversAbandonedGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX protocol execution requires sh")
	}
	home := t.TempDir()
	key := workspaceOwnerKey("cbx_stale_gate")
	token := strings.Repeat("d", 64)
	root := filepath.Join(home, ".crabbox", "workspace-owners")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, key+".gate"), []byte("abandoned"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: 30 * time.Second}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "ACQUIRED" {
		t.Fatalf("recover abandoned gate out=%q err=%v", out, err)
	}
	req.Action = workspaceOwnerRelease
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(req)); err != nil || out != "RELEASED" {
		t.Fatalf("release after gate recovery out=%q err=%v", out, err)
	}
}

func TestWorkspaceOwnerWindowsProtocolBehavior(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native Windows protocol execution runs in Windows CI")
	}
	key := workspaceOwnerKey("cbx_windows_protocol_" + strconv.FormatInt(time.Now().UnixNano(), 10))
	token := strings.Repeat("c", 64)
	prepare := `$root = Join-Path $HOME ".crabbox\workspace-owners"
New-Item -ItemType Directory -Force -Path $root | Out-Null
Set-Content -LiteralPath (Join-Path $root (` + psQuote(key) + ` + ".gate")) -Value "abandoned" -Encoding ASCII
`
	if out, err := runWindowsPowerShellScript(t, prepare); err != nil {
		t.Fatalf("prepare abandoned Windows gate out=%q err=%v", out, err)
	}
	req := workspaceOwnerRemoteRequest{Action: workspaceOwnerAcquire, Key: key, Token: token, TTL: 30 * time.Second}
	if out, err := runWindowsPowerShellScript(t, remoteWorkspaceOwnerWindows(req)); err != nil || !strings.Contains(string(out), "ACQUIRED") {
		t.Fatalf("Windows acquire out=%q err=%v", out, err)
	}
	if out, err := runWindowsPowerShellScript(t, remoteWorkspaceOwnerWindows(req)); err != nil || !strings.Contains(string(out), "ACQUIRED") {
		t.Fatalf("Windows same-token acquire replay out=%q err=%v", out, err)
	}
	expire := `$root = Join-Path $HOME ".crabbox\workspace-owners"
$state = Join-Path $root (` + psQuote(key) + ` + ".owner")
Set-Content -LiteralPath $state -Value @("v1", ` + psQuote(token) + `, "1") -Encoding ASCII
`
	if out, err := runWindowsPowerShellScript(t, expire); err != nil {
		t.Fatalf("expire Windows owner state out=%q err=%v", out, err)
	}
	req.Action = workspaceOwnerRenew
	if out, err := runWindowsPowerShellScript(t, remoteWorkspaceOwnerWindows(req)); err == nil || !strings.Contains(string(out), "EXPIRED") {
		t.Fatalf("late Windows renew out=%q err=%v", out, err)
	}
	req.Action = workspaceOwnerInspect
	if out, err := runWindowsPowerShellScript(t, remoteWorkspaceOwnerWindows(req)); err == nil || !strings.Contains(string(out), "EXPIRED") {
		t.Fatalf("late Windows inspect out=%q err=%v", out, err)
	}
	if out, err := runWindowsPowerShellScript(t, remoteWorkspaceOwnerWindowsWitness(key, token, "Write-Output late-child", nil)); err == nil || strings.Contains(string(out), "late-child") {
		t.Fatalf("late Windows witness executed: out=%q err=%v", out, err)
	}
	verifyExpired := `$root = Join-Path $HOME ".crabbox\workspace-owners"
$state = Join-Path $root (` + psQuote(key) + ` + ".owner")
$child = Join-Path $root (` + psQuote(key) + ` + ".child")
$lines = @(Get-Content -LiteralPath $state -ErrorAction Stop)
if ($lines.Count -ne 3 -or $lines[2] -ne "1") { throw "expired state changed" }
if (Test-Path -LiteralPath $child) { throw "late witness published child" }
`
	if out, err := runWindowsPowerShellScript(t, verifyExpired); err != nil {
		t.Fatalf("verify expired Windows generation out=%q err=%v", out, err)
	}
	req.Action = workspaceOwnerAcquire
	if out, err := runWindowsPowerShellScript(t, remoteWorkspaceOwnerWindows(req)); err != nil || !strings.Contains(string(out), "RECOVERED") {
		t.Fatalf("recover expired Windows generation out=%q err=%v", out, err)
	}
	if out, err := runWindowsPowerShellScript(t, remoteWorkspaceOwnerWindowsWitness(key, token, "Write-Output child-ok", nil)); err != nil || !strings.Contains(string(out), "child-ok") {
		t.Fatalf("Windows witnessed child out=%q err=%v", out, err)
	}
	if out, err := runWindowsPowerShellScript(t, remoteWorkspaceOwnerWindowsWitness(key, token, "exit 23", nil)); err == nil || exitCode(err) != 23 {
		t.Fatalf("Windows nonzero witnessed child out=%q err=%v", out, err)
	}
	descendantState := filepath.Join(t.TempDir(), "descendant-state")
	rejectedPath := filepath.Join(t.TempDir(), "rejected-overlap")
	descendantPayload := `$descendant = Start-Process -FilePath "powershell.exe" -ArgumentList @("-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "Start-Sleep -Milliseconds 1500") -WindowStyle Hidden -PassThru
[IO.File]::WriteAllText(` + psQuote(descendantState) + `, ([string]$PID + [Environment]::NewLine + [string]$descendant.Id))
exit 42`
	powerShell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal(err)
	}
	witnessScript := filepath.Join(t.TempDir(), "descendant-witness.ps1")
	if err := os.WriteFile(witnessScript, []byte(remoteWorkspaceOwnerWindowsWitness(key, token, descendantPayload, nil)), 0o644); err != nil {
		t.Fatal(err)
	}
	var descendantOutput bytes.Buffer
	descendantWitness := exec.Command(powerShell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", witnessScript)
	descendantWitness.Stdout = &descendantOutput
	descendantWitness.Stderr = &descendantOutput
	started := time.Now()
	if err := descendantWitness.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = descendantWitness.Process.Kill() })
	var leaderPID int
	deadline := time.Now().Add(5 * time.Second)
	for leaderPID == 0 {
		data, _ := os.ReadFile(descendantState)
		fields := strings.Fields(string(data))
		if len(fields) == 2 {
			leaderPID, _ = strconv.Atoi(fields[0])
		}
		if time.Now().After(deadline) {
			t.Fatalf("Windows descendant payload did not publish PIDs: %q", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		checkLeader := `if (Get-Process -Id ` + strconv.Itoa(leaderPID) + ` -ErrorAction SilentlyContinue) { exit 1 }`
		if _, err := runWindowsPowerShellScript(t, checkLeader); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Windows workload leader %d remained live", leaderPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	req.Action = workspaceOwnerInspect
	if out, err := runWindowsPowerShellScript(t, remoteWorkspaceOwnerWindows(req)); err != nil || !strings.Contains(string(out), "CHILD") {
		t.Fatalf("Windows descendant-only inspect out=%q err=%v", out, err)
	}
	if out, err := runWindowsPowerShellScript(t, remoteWorkspaceOwnerWindowsWitness(key, token, `[IO.File]::WriteAllText(`+psQuote(rejectedPath)+`, "bad")`, nil)); err == nil || exitCode(err) != 75 {
		t.Fatalf("Windows overlapping descendant witness out=%q err=%v", out, err)
	}
	if err := descendantWitness.Wait(); err == nil || exitCode(err) != 42 {
		t.Fatalf("Windows descendant witness out=%q err=%v", descendantOutput.String(), err)
	}
	if time.Since(started) < time.Second {
		t.Fatal("Windows normal completion did not wait for its job descendants")
	}
	if _, err := os.Stat(rejectedPath); !os.IsNotExist(err) {
		t.Fatalf("Windows overlapping descendant witness executed: %v", err)
	}
	req.Action = workspaceOwnerRelease
	if out, err := runWindowsPowerShellScript(t, remoteWorkspaceOwnerWindows(req)); err != nil || !strings.Contains(string(out), "RELEASED") {
		t.Fatalf("Windows release out=%q err=%v", out, err)
	}
}
