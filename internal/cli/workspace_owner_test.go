package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
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
	ambiguousRelease bool
	blockBusyAcquire bool
	changed          chan struct{}
}

type workspaceOwnerTransportFunc func(context.Context, workspaceOwnerRemoteRequest) (string, error)

func (f workspaceOwnerTransportFunc) Do(ctx context.Context, req workspaceOwnerRemoteRequest) (string, error) {
	return f(ctx, req)
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
			if f.token == "" {
				f.token = req.Token
				f.expires = time.Now().Add(req.TTL)
				f.signalLocked()
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
			if f.failRenew {
				f.mu.Unlock()
				return "", errors.New("renew transport lost")
			}
			if f.token != req.Token {
				f.mu.Unlock()
				return "MISMATCH", nil
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

func TestWorkspaceOwnerAcquisitionBoundary(t *testing.T) {
	nonExclusive := newWatchTestBackend()
	if !shouldAcquireWorkspaceOwner(SSHTarget{}, true, false, nonExclusive) {
		t.Fatal("a successful non-exclusive acquisition must acquire the workspace owner")
	}
	exclusive := runEnvProfileTestBackend{}
	tests := []struct {
		name      string
		target    SSHTarget
		acquired  bool
		mayRetain bool
		wantOwner bool
	}{
		{name: "fresh Linux one-shot cleanup", target: SSHTarget{TargetOS: targetLinux}, acquired: true},
		{name: "fresh macOS one-shot cleanup", target: SSHTarget{TargetOS: targetMacOS}, acquired: true},
		{name: "fresh WSL2 one-shot cleanup", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, acquired: true},
		{name: "fresh Windows input requires witness", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, acquired: true, wantOwner: true},
		{name: "fresh keep", acquired: true, mayRetain: true, wantOwner: true},
		{name: "fresh keep-on-failure", acquired: true, mayRetain: true, wantOwner: true},
		{name: "reused retained lease", acquired: false, wantOwner: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldAcquireWorkspaceOwner(tt.target, tt.acquired, tt.mayRetain, exclusive); got != tt.wantOwner {
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
	if got, want := prepared.command, owner.wrapPOSIXCommand("printf ok", false, prepared.setupMarker); got != want || prepared.setupMarker == "" {
		t.Fatal("ordinary SSH child was not witnessed with setup diagnostics")
	}
	if strings.Contains(prepared.command, "\n") || strings.Count(prepared.command, "'") != 2 {
		t.Fatal("diagnostic launcher lost login-shell-independent quoting")
	}
	if script := remoteWorkspaceOwnerPOSIXWitnessScript(owner.key, owner.token, "printf ok", ""); !strings.Contains(script, "child_identity=$(ps -o lstart=") {
		t.Fatalf("ordinary SSH child witness lost identity fencing:\n%s", script)
	}
	inputSize := int64(0)
	prepared, err = prepareWorkspaceOwnerRemote(ctx, owner.target, "cat", &inputSize)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := prepared.command, owner.wrapPOSIXCommand("cat", true, prepared.setupMarker); got != want || prepared.setupMarker == "" {
		t.Fatal("input SSH child did not preserve stdin and setup diagnostics")
	}
	if script := remoteWorkspaceOwnerPOSIXWitnessScript(owner.key, owner.token, "cat", "", true); !strings.Contains(script, `cat >"$run_dir/input"`) || !strings.Contains(script, `<"$run_dir/input"`) {
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
			if !shouldAcquireWorkspaceOwner(SSHTarget{}, false, false, nil) {
				t.Fatalf("reused %s path bypassed workspace ownership", lifecycle)
			}
		})
	}
}

func TestWorkspaceOwnerSerializesStaticRunAndStandaloneActionsHydration(t *testing.T) {
	if !shouldAcquireWorkspaceOwner(SSHTarget{}, true, false, testStaticSSHBackend{}) {
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
	for _, want := range []string{".crabbox/workspace-owners", key + ".gate", "$key.owner", "$key.child", "flock -x -w 0", "lockf -t 0", "ps -o lstart=", "RECOVERED", "MISMATCH", "EXPIRED", "AMBIGUOUS", `[ "$state_expiry" -gt "$(date +%s)" ]`} {
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
	posixWitness := remoteWorkspaceOwnerPOSIXWitnessScript(key, token, "printf ok", "")
	for _, want := range []string{"child_identity=$(ps -o lstart=", "owner_expiry=$(sed -n", "owner_expiry", "date +%s", "mv \"$child_tmp\" \"$child\"", "rm -f \"$child\""} {
		if !strings.Contains(posixWitness, want) {
			t.Fatalf("POSIX child witness missing %q:\n%s", want, posixWitness)
		}
	}
	windowsWitness := remoteWorkspaceOwnerWindowsWitness(key, token, "Write-Output ok", nil)
	for _, want := range []string{"Start-Process", "$null = $process.Handle", "StartTime.ToUniversalTime().Ticks", "Read-Expiry", "(Read-Expiry) -le [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()", "Move-Item -LiteralPath $tmp -Destination $child", "$payloadExitCode = $global:LASTEXITCODE", "if (-not $payloadSucceeded) { exit 1 }", "$process.WaitForExit()", "Remove-Item -LiteralPath $child"} {
		if !strings.Contains(windowsWitness, want) {
			t.Fatalf("Windows child witness missing %q:\n%s", want, windowsWitness)
		}
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
port=
previous=
for arg do
  if [ "$previous" = "-p" ]; then port=$arg; fi
  previous=$arg
  command=$arg
done
printf '%s' "$command" > "$log_dir/$count.command"
printf '%s\n' "$@" > "$log_dir/$count.args"
if [ -n "${CRABBOX_OWNER_SSH_REAL_PORT:-}" ] && [ "$port" = "$CRABBOX_OWNER_SSH_REAL_PORT" ]; then
  : > "$log_dir/$count.stdin"
  exec "$CRABBOX_OWNER_SSH_REAL_EXECUTABLE" -F /dev/null "$@"
fi
if [ "$command" = "exit 0" ] && [ "${CRABBOX_OWNER_SSH_FAIL_PROBES:-}" = 1 ]; then
  : > "$log_dir/$count.stdin"
  printf 'probe-%s %s\n' "$port" "${CRABBOX_OWNER_SSH_PROBE_SECRET:-}" >&2
  exit 255
fi
if [ "$command" != "exit 0" ] && [ "${CRABBOX_OWNER_SSH_DELIVERY_255:-}" = partial ]; then
  /bin/dd bs=1 count=1 > "$log_dir/$count.stdin" 2>/dev/null
  exit 255
fi
/bin/cat > "$log_dir/$count.stdin"
if [ "$command" != "exit 0" ] && [ "${CRABBOX_OWNER_SSH_FAIL_FIRST_DELIVERY:-}" = 1 ] &&
   [ ! -e "$log_dir/failed-delivery" ]; then
  : > "$log_dir/failed-delivery"
  exit "${CRABBOX_OWNER_SSH_FAIL_CODE:-7}"
fi
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
if [ "$command" != "exit 0" ] && [ "${CRABBOX_OWNER_SSH_DELIVERY_255:-}" = full ]; then
  exit 255
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

func requireWorkspaceOwnerSSHNoMux(t *testing.T, dir string, index int) {
	t.Helper()
	args, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(index)+".args"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(args)
	for _, want := range []string{"ControlMaster=no", "ControlPath=none", "ControlPersist=no"} {
		if !strings.Contains(text, want) {
			t.Fatalf("SSH call %d missing %q in %q", index, want, text)
		}
	}
	for _, unwanted := range []string{"ControlMaster=auto", "ControlPersist=10m"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("SSH call %d retained %q in %q", index, unwanted, text)
		}
	}
}

func requireWorkspaceOwnerSSHOptions(t *testing.T, dir string, index int, connectTimeout, connectionAttempts string) {
	t.Helper()
	args, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(index)+".args"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(args)
	for _, want := range []string{"ConnectTimeout=" + connectTimeout, "ConnectionAttempts=" + connectionAttempts} {
		if !strings.Contains(text, want) {
			t.Fatalf("SSH call %d missing %q in %q", index, want, text)
		}
	}
}

func requireWorkspaceOwnerSSHProbe(t *testing.T, dir string, index int, port string) {
	t.Helper()
	command, input := readWorkspaceOwnerSSHCall(t, dir, index)
	if command != "exit 0" || input != "" {
		t.Fatalf("SSH call %d command=%q stdin=%q, want zero-byte probe", index, command, input)
	}
	args, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(index)+".args"))
	if err != nil {
		t.Fatal(err)
	}
	text := "\n" + string(args)
	if !strings.Contains(text, "\n-n\n") || !strings.Contains(text, "\n-p\n"+port+"\n") {
		t.Fatalf("SSH call %d probe args=%q, want -n on port %s", index, text, port)
	}
}

func TestSSHSingletonPortExecutesOnceWithoutProbe(t *testing.T) {
	tests := []struct {
		name   string
		target SSHTarget
		port   string
	}{
		{name: "default", target: SSHTarget{}, port: "22"},
		{name: "configured", target: SSHTarget{Port: "2222", FallbackPorts: []string{}}, port: "2222"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := installWorkspaceOwnerRecordingSSH(t)
			test.target.User, test.target.Host = "crabbox", "example.test"
			payload := "one delivery\n"
			if err := runSSHInput(t.Context(), test.target, "cat", strings.NewReader(payload), io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			if count, err := os.ReadFile(filepath.Join(dir, "count")); err != nil || string(count) != "1" {
				t.Fatalf("SSH call count=%q err=%v, want one delivery", count, err)
			}
			command, input := readWorkspaceOwnerSSHCall(t, dir, 1)
			if command != "cat" || input != payload {
				t.Fatalf("command=%q stdin=%q", command, input)
			}
			args, err := os.ReadFile(filepath.Join(dir, "1.args"))
			if err != nil {
				t.Fatal(err)
			}
			if text := "\n" + string(args); !strings.Contains(text, "\n-p\n"+test.port+"\n") || strings.Contains(text, "\n-p\n\n") {
				t.Fatalf("SSH args=%q, want pinned port %s", text, test.port)
			}
		})
	}
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
			if isWindowsWSL2Target(test.target) {
				oldStage := stageWSLSpool
				stageWSLSpool = func(spool *wslStageSpool, _ context.Context, target *SSHTarget, _ wslStageTiming, _, _ string, _ io.Writer) (string, error) {
					spool.shell = wslStageCMD
					target.NoControlMaster = true
					return strings.Repeat("a", 32), nil
				}
				t.Cleanup(func() { stageWSLSpool = oldStage })
			}
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
			if isWindowsWSL2Target(test.target) {
				oldStage, oldCleanup := stageWSLSpool, cleanupPublishedWSLStage
				stageWSLSpool = func(spool *wslStageSpool, _ context.Context, target *SSHTarget, _ wslStageTiming, _, _ string, _ io.Writer) (string, error) {
					spool.shell = wslStageCMD
					target.NoControlMaster = true
					return strings.Repeat("a", 32), nil
				}
				cleanupPublishedWSLStage = func(context.Context, SSHTarget, string, *wslStageSpool, time.Duration, time.Duration, string) error {
					return nil
				}
				t.Cleanup(func() { stageWSLSpool, cleanupPublishedWSLStage = oldStage, oldCleanup })
			}
			secret := strings.Repeat("s", 80)
			requestToken := strings.Repeat("b", 64)
			t.Setenv("CRABBOX_OWNER_SSH_FAIL_CALL", "1")
			t.Setenv("CRABBOX_OWNER_SSH_FAIL_CODE", "23")
			t.Setenv("CRABBOX_OWNER_SSH_FAIL_STDOUT", "MISMATCH")
			t.Setenv("CRABBOX_OWNER_SSH_FAIL_STDERR", strings.Repeat("x", 190)+" owner-token="+requestToken+" Authorization: Bearer "+secret+" "+strings.Repeat("z", 300)+"\n")
			test.target.User, test.target.Host, test.target.Port = "crabbox", "127.0.0.1", "22"
			got, err := (sshWorkspaceOwnerTransport{target: test.target}).Do(context.Background(), workspaceOwnerRemoteRequest{
				Action: workspaceOwnerRenew,
				Key:    workspaceOwnerKey("cbx_failure_" + test.name),
				Token:  requestToken,
				TTL:    time.Minute,
			})
			if got != "MISMATCH" || err == nil || exitCode(err) != 23 {
				t.Fatalf("response=%q err=%v exit=%d", got, err, exitCode(err))
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), requestToken) || !strings.Contains(err.Error(), diagnosticRedaction) {
				t.Fatalf("failure diagnostic was not redacted: %q", err)
			}
			if detail := strings.TrimPrefix(err.Error(), "exit status 23: "); len(detail) > 243 {
				t.Fatalf("failure detail length=%d want <=243: %q", len(detail), detail)
			}
		})
	}
}

func TestWorkspaceOwnerSSHProtocolFailureRedactsAuthSecretUser(t *testing.T) {
	installWorkspaceOwnerRecordingSSH(t)
	secret := "token-as-username-must-not-leak"
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_CALL", "1")
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_STDERR", secret+"@example.test: Permission denied\n")
	target := SSHTarget{
		User:       secret,
		Host:       "127.0.0.1",
		Port:       "22",
		TargetOS:   targetLinux,
		AuthSecret: true,
	}
	_, err := (sshWorkspaceOwnerTransport{target: target}).Do(context.Background(), workspaceOwnerRemoteRequest{
		Action: workspaceOwnerRenew,
		Key:    workspaceOwnerKey("cbx_failure_auth_secret"),
		Token:  strings.Repeat("b", 64),
		TTL:    time.Minute,
	})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), diagnosticRedaction) {
		t.Fatalf("AuthSecret username was not redacted: %q", err)
	}
}

func TestWorkspaceOwnerSSHProtocolAllProbeFailureRedactsFinalDiagnostic(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	secret := "token-as-username-must-not-leak"
	requestToken := strings.Repeat("b", 64)
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_PROBES", "1")
	t.Setenv("CRABBOX_OWNER_SSH_PROBE_SECRET", secret+" "+requestToken)
	target := SSHTarget{
		User: secret, Host: "example.test", Port: "2222", FallbackPorts: []string{"22"},
		TargetOS: targetLinux, AuthSecret: true,
	}
	_, err := (sshWorkspaceOwnerTransport{target: target}).Do(t.Context(), workspaceOwnerRemoteRequest{
		Action: workspaceOwnerRenew,
		Key:    workspaceOwnerKey("cbx_probe_failure"),
		Token:  requestToken,
		TTL:    time.Minute,
	})
	if err == nil || exitCode(err) != 255 {
		t.Fatalf("err=%v exit=%d, want 255", err, exitCode(err))
	}
	if strings.Contains(err.Error(), "probe-2222") || !strings.Contains(err.Error(), "probe-22") {
		t.Fatalf("owner diagnostic=%q, want only final probe", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), requestToken) || !strings.Contains(err.Error(), diagnosticRedaction) {
		t.Fatalf("owner probe diagnostic was not redacted: %q", err)
	}
	requireWorkspaceOwnerSSHProbe(t, dir, 1, "2222")
	requireWorkspaceOwnerSSHProbe(t, dir, 2, "22")
}

func TestWorkspaceOwnerSSHProtocolResolvesFallbackBeforeSingleDelivery(t *testing.T) {
	tests := []struct {
		name   string
		target SSHTarget
	}{
		{name: "POSIX", target: SSHTarget{TargetOS: targetLinux}},
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
			if count, readErr := os.ReadFile(filepath.Join(dir, "count")); readErr != nil || string(count) != "3" {
				t.Fatalf("SSH call count=%q err=%v", count, readErr)
			}
			requireWorkspaceOwnerSSHProbe(t, dir, 1, "2222")
			requireWorkspaceOwnerSSHProbe(t, dir, 2, "22")
			command, _ := readWorkspaceOwnerSSHCall(t, dir, 3)
			if command == "exit 0" {
				t.Fatal("owner delivery was replaced by another probe")
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

func TestWorkspaceOwnerWSL2StagesThenExecutesOnceWithoutStdin(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	nonce := strings.Repeat("a", 32)
	staged := 0
	var launchers []string
	wantTiming := sshTransportTiming(sshCommandLimit{execution: workspaceOwnerRemoteTimeout, control: true})
	captureWSLStage(t, nonce, func(spool *wslStageSpool, target *SSHTarget, timing wslStageTiming, data []byte) {
		staged++
		if timing != wantTiming || !target.NoControlMaster {
			t.Fatalf("timing=%+v no-mux=%t", timing, target.NoControlMaster)
		}
		if len(data) != int(spool.size) || string(data[:8]) != "CBXFLAT2" {
			t.Fatalf("invalid staged owner spool bytes=%d", len(data))
		}
		if spool.size > 1<<20 || wslStageBudget(timing.stage, timing.idle, spool.size) != sshTransportTiming(sshCommandLimit{execution: workspaceOwnerRemoteTimeout, control: true}).stage {
			t.Fatalf("owner spool exceeds its accounted upload phase: bytes=%d budget=%s", spool.size, wslStageBudget(timing.stage, timing.idle, spool.size))
		}
		if binary.LittleEndian.Uint64(data[32:]) != uint64(timing.operation.Milliseconds()) || timing.operation != 38*time.Second {
			t.Fatal("derived control operation guard missing")
		}
		launchers = append(launchers, wslStageLauncherCommand(nonce, spool.size, spool.digest(), wslStageCMD))
	})

	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	key, token := workspaceOwnerKey("cbx_wsl2_staged_protocol"), strings.Repeat("6", 64)
	for index, action := range []workspaceOwnerAction{workspaceOwnerAcquire, workspaceOwnerRenew, workspaceOwnerInspect, workspaceOwnerRelease} {
		want := map[workspaceOwnerAction]string{
			workspaceOwnerAcquire: "ACQUIRED", workspaceOwnerRenew: "RENEWED",
			workspaceOwnerInspect: "OWNED", workspaceOwnerRelease: "RELEASED",
		}[action]
		t.Setenv("CRABBOX_OWNER_SSH_SUCCESS_STDOUT", want)
		got, err := (sshWorkspaceOwnerTransport{target: target}).Do(t.Context(), workspaceOwnerRemoteRequest{
			Action: action, Key: key, Token: token, TTL: time.Minute,
		})
		if err != nil || got != want {
			t.Fatalf("%s response=%q err=%v", action, got, err)
		}
		command, input := readWorkspaceOwnerSSHCall(t, dir, index+1)
		if command != launchers[index] || input != "" {
			t.Fatalf("%s command_match=%t stdin=%q", action, command == launchers[index], input)
		}
		if len(command) >= wslStageLauncherCommandLimit {
			t.Fatalf("%s launcher bytes=%d exceeds Windows command limit", action, len(command))
		}
		decoded := decodePowerShellCommand(t, command)
		if strings.Contains(decoded, key) || strings.Contains(decoded, token) {
			t.Fatalf("%s leaked owner data in argv", action)
		}
		requireWorkspaceOwnerSSHNoMux(t, dir, index+1)
		requireWorkspaceOwnerSSHOptions(t, dir, index+1, "10", "3")
	}
	if staged != 4 {
		t.Fatalf("stage calls=%d want 4", staged)
	}
}

func TestWorkspaceOwnerWSL2AmbiguousExecuteDoesNotRestage(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	oldStage, oldCleanup := stageWSLSpool, cleanupPublishedWSLStage
	staged, cleaned := 0, 0
	stageWSLSpool = func(spool *wslStageSpool, _ context.Context, _ *SSHTarget, _ wslStageTiming, _, _ string, _ io.Writer) (string, error) {
		spool.shell = wslStageCMD
		staged++
		return strings.Repeat("b", 32), nil
	}
	cleanupPublishedWSLStage = func(_ context.Context, _ SSHTarget, nonce string, _ *wslStageSpool, _, _ time.Duration, _ string) error {
		cleaned++
		if nonce != strings.Repeat("b", 32) {
			t.Fatalf("ambiguous delivery cleaned an unowned nonce: %q", nonce)
		}
		return nil
	}
	t.Cleanup(func() { stageWSLSpool, cleanupPublishedWSLStage = oldStage, oldCleanup })
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_CALL", "1")
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_CODE", "255")
	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	_, err := (sshWorkspaceOwnerTransport{target: target}).Do(t.Context(), workspaceOwnerRemoteRequest{
		Action: workspaceOwnerAcquire, Key: workspaceOwnerKey("cbx_wsl2_ambiguous"), Token: strings.Repeat("7", 64), TTL: time.Minute,
	})
	var exitErr ExitError
	if err == nil || !AsExitError(err, &exitErr) || exitErr.Code != 7 || staged != 1 || cleaned != 1 {
		t.Fatalf("err=%v exit=%+v stages=%d cleanups=%d", err, exitErr, staged, cleaned)
	}
	if count, readErr := os.ReadFile(filepath.Join(dir, "count")); readErr != nil || string(count) != "1" {
		t.Fatalf("SSH calls=%q err=%v", count, readErr)
	}
}

func TestWorkspaceOwnerWANBudgetContract(t *testing.T) {
	for _, test := range []struct {
		name   string
		target SSHTarget
		routes int
	}{
		{"default port", SSHTarget{}, 1},
		{"explicit singleton", SSHTarget{Port: "2222", FallbackPorts: []string{}}, 1},
		{"default fallback", SSHTarget{Port: "2222"}, 2},
		{"multiple deduplicated fallbacks", SSHTarget{Port: "2222", FallbackPorts: []string{"22", "2200", "22"}}, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := test.target
			target.TargetOS, target.WindowsMode = "windows", "wsl2"
			timing := sshTransportTiming(sshCommandLimit{execution: workspaceOwnerRemoteTimeout, control: true})
			stage := wslStageRouteBudgets(target, timing, 1<<20)
			// Each route has ACL prep, one complete WAN upload, exact cleanup,
			// and separate upload/cleanup subprocess completion bounds.
			candidate := 47*time.Second + 59*time.Second + 15*time.Second + 2*sshCommandWaitDelay
			if len(stage.ports) != test.routes || stage.candidate != candidate || stage.total != time.Duration(test.routes)*candidate+wslStageCompletionMargin {
				t.Fatalf("route allocation=%+v", stage)
			}
			call := sshTransportCallBudget(target, sshControlMetadataLimit, sshCommandLimit{execution: workspaceOwnerRemoteTimeout, control: true})
			if timing.execute != 110*time.Second || timing.reserve != 131*time.Second || call != time.Duration(131*test.routes+132)*time.Second {
				t.Fatalf("control allocation: timing=%+v call=%s", timing, call)
			}
			lifecycle := time.Duration(test.routes)*candidate + timing.execute + wslStageCleanupTimeout + sshCommandWaitDelay
			if call <= lifecycle || call != stage.total+timing.reserve {
				t.Fatalf("call=%s lifecycle=%s", call, lifecycle)
			}
			owner := &workspaceOwner{transport: sshWorkspaceOwnerTransport{target: target}}
			if owner.callTimeout() != call || owner.quiesceTimeout() != 2*call+time.Second {
				t.Fatal("owner caller lost dynamic route allocation")
			}
			ttl := workspaceOwnerRenewInterval + call + workspaceOwnerRenewMargin
			wait := ttl + workspaceOwnerPollInterval + call + workspaceOwnerRenewMargin
			if ttl != call+11*time.Second || wait != 2*call+13*time.Second || ttl <= workspaceOwnerRenewInterval+lifecycle {
				t.Fatal("renewal exceeds TTL")
			}
		})
	}
}

func TestWorkspaceOwnerAcquisitionAllowsCompleteCallAfterStaleExpiry(t *testing.T) {
	calls := 0
	var postExpiryBudget time.Duration
	transport := workspaceOwnerTransportFunc(func(ctx context.Context, req workspaceOwnerRemoteRequest) (string, error) {
		if req.Action == workspaceOwnerRelease {
			return "RELEASED", nil
		}
		calls++
		if calls == 1 {
			return "BUSY", nil
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("post-expiry acquisition has no bounded deadline")
		}
		postExpiryBudget = time.Until(deadline)
		time.Sleep(75 * time.Millisecond)
		return "RECOVERED", nil
	})
	owner, err := acquireWorkspaceOwnerWithTransport(t.Context(), SSHTarget{}, "cbx_stale_owner_wan", io.Discard, transport,
		workspaceOwnerPollInterval+300*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond)
	if err != nil || calls != 2 || postExpiryBudget < 200*time.Millisecond {
		t.Fatalf("owner=%v calls=%d post-expiry budget=%s error=%v", owner, calls, postExpiryBudget, err)
	}
	if err := owner.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceOwnerAcquireBoundsTransportCalls(t *testing.T) {
	t.Run("call budget", func(t *testing.T) {
		calls := 0
		var remaining time.Duration
		transport := workspaceOwnerTransportFunc(func(ctx context.Context, _ workspaceOwnerRemoteRequest) (string, error) {
			calls++
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("acquire transport call has no deadline")
			}
			remaining = time.Until(deadline)
			return "", errors.New("delivery outcome lost")
		})
		_, err := acquireWorkspaceOwnerWithTransport(context.Background(), SSHTarget{}, "cbx_acquire_call_budget", io.Discard, transport, workspaceOwnerWaitTimeout, time.Minute, 10*time.Second)
		if calls != 1 || remaining <= workspaceOwnerCallTimeout-time.Second || remaining > workspaceOwnerCallTimeout || err == nil || !strings.Contains(err.Error(), "ambiguous remote state") {
			t.Fatalf("calls=%d remaining=%s err=%v", calls, remaining, err)
		}
	})

	t.Run("smaller overall deadline stays ambiguous", func(t *testing.T) {
		calls := 0
		var remaining time.Duration
		started := time.Now()
		transport := workspaceOwnerTransportFunc(func(ctx context.Context, _ workspaceOwnerRemoteRequest) (string, error) {
			calls++
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("acquire transport call has no deadline")
			}
			remaining = time.Until(deadline)
			<-ctx.Done()
			return "", ctx.Err()
		})
		_, err := acquireWorkspaceOwnerWithTransport(context.Background(), SSHTarget{}, "cbx_acquire_delivery_timeout", io.Discard, transport, 50*time.Millisecond, time.Minute, 10*time.Second)
		elapsed := time.Since(started)
		var exitErr ExitError
		if calls != 1 || remaining <= 0 || remaining > 100*time.Millisecond || elapsed >= time.Second || !AsExitError(err, &exitErr) || exitErr.Code != 7 || !strings.Contains(err.Error(), "ambiguous remote state") || strings.Contains(err.Error(), "timed out after") {
			t.Fatalf("calls=%d remaining=%s elapsed=%s err=%v", calls, remaining, elapsed, err)
		}
	})

	t.Run("definitive busy uses outer wait deadline", func(t *testing.T) {
		calls := 0
		transport := workspaceOwnerTransportFunc(func(context.Context, workspaceOwnerRemoteRequest) (string, error) {
			calls++
			return "BUSY", nil
		})
		_, err := acquireWorkspaceOwnerWithTransport(context.Background(), SSHTarget{}, "cbx_acquire_busy", io.Discard, transport, 40*time.Millisecond, time.Minute, 10*time.Second)
		var exitErr ExitError
		if calls != 1 || !AsExitError(err, &exitErr) || exitErr.Code != 7 || !strings.Contains(err.Error(), "timed out after") || strings.Contains(err.Error(), "ambiguous remote state") {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
}

func TestWorkspaceOwnerOperationDeadlineTable(t *testing.T) {
	tests := []struct {
		name       string
		action     workspaceOwnerAction
		wantBudget time.Duration
		response   string
		invoke     func(*workspaceOwner) error
	}{
		{name: "acquire", action: workspaceOwnerAcquire, wantBudget: workspaceOwnerCallTimeout, response: "lost", invoke: func(owner *workspaceOwner) error {
			_, err := acquireWorkspaceOwnerWithTransport(context.Background(), SSHTarget{}, "cbx_deadline_table", io.Discard, owner.transport, workspaceOwnerWaitTimeout, workspaceOwnerTTL, workspaceOwnerRenewInterval)
			return err
		}},
		{name: "renew", action: workspaceOwnerRenew, wantBudget: workspaceOwnerCallTimeout, response: "lost", invoke: func(owner *workspaceOwner) error {
			ticks := make(chan time.Time, 1)
			ticks <- time.Now()
			owner.renewLoopWithTicks(ticks, workspaceOwnerCallTimeout)
			return owner.Err()
		}},
		{name: "inspect", action: workspaceOwnerInspect, wantBudget: workspaceOwnerCallTimeout, response: "OWNED", invoke: func(owner *workspaceOwner) error {
			_, err := owner.inspectChild(context.Background())
			return err
		}},
		{name: "witness", action: workspaceOwnerInspect, wantBudget: workspaceOwnerCallTimeout, response: "CHILD", invoke: func(owner *workspaceOwner) error {
			return owner.WaitForChild(context.Background(), workspaceOwnerCallTimeout)
		}},
		{name: "release", action: workspaceOwnerRelease, wantBudget: workspaceOwnerCallTimeout, response: "RELEASED", invoke: func(owner *workspaceOwner) error {
			close(owner.done)
			return owner.Close(context.Background())
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotAction workspaceOwnerAction
			var gotBudget time.Duration
			ownerCtx, cancel := context.WithCancel(context.Background())
			owner := &workspaceOwner{
				transport: workspaceOwnerTransportFunc(func(ctx context.Context, req workspaceOwnerRemoteRequest) (string, error) {
					gotAction = req.Action
					deadline, ok := ctx.Deadline()
					if !ok {
						t.Fatal("owner transport call has no deadline")
					}
					gotBudget = time.Until(deadline)
					if test.response == "lost" {
						return "", errors.New("delivery outcome lost")
					}
					return test.response, nil
				}),
				ttl: workspaceOwnerTTL, ctx: ownerCtx, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{}),
			}
			err := test.invoke(owner)
			if gotAction != test.action || gotBudget <= test.wantBudget-time.Second || gotBudget > test.wantBudget ||
				test.response != "lost" && err != nil || test.response == "lost" && err == nil {
				t.Fatalf("action=%s budget=%s err=%v want action=%s budget=%s", gotAction, gotBudget, err, test.action, test.wantBudget)
			}
		})
	}
}

func TestWaitWorkspaceOwnerNoChildUsesTypedResult(t *testing.T) {
	tests := []struct {
		name      string
		responses []string
		err       error
		timeout   time.Duration
		wantCalls int
		wantErr   string
	}{
		{name: "child then owned", responses: []string{"CHILD", "OWNED"}, timeout: time.Second, wantCalls: 2},
		{name: "persistent child", responses: []string{"CHILD"}, timeout: 10 * time.Millisecond, wantCalls: 1, wantErr: "failed closed: child"},
		{name: "transport ambiguity mentions child", err: errors.New("child route disconnected"), timeout: time.Second, wantCalls: 1, wantErr: "ambiguous remote state"},
		{name: "malformed response", responses: []string{"CHILDISH"}, timeout: time.Second, wantCalls: 1, wantErr: "childish"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			owner := &workspaceOwner{transport: workspaceOwnerTransportFunc(func(_ context.Context, _ workspaceOwnerRemoteRequest) (string, error) {
				calls++
				if tt.err != nil {
					return "", tt.err
				}
				return tt.responses[min(calls-1, len(tt.responses)-1)], nil
			})}
			err := waitWorkspaceOwnerNoChild(context.Background(), owner, tt.timeout)
			if calls != tt.wantCalls || (tt.wantErr == "") != (err == nil) || tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("calls=%d err=%v want calls=%d error containing %q", calls, err, tt.wantCalls, tt.wantErr)
			}
		})
	}
}

func TestWaitWorkspaceOwnerNoChildBoundsInspectToDeadline(t *testing.T) {
	calls := 0
	owner := &workspaceOwner{transport: workspaceOwnerTransportFunc(func(ctx context.Context, _ workspaceOwnerRemoteRequest) (string, error) {
		calls++
		if calls == 1 {
			return "CHILD", nil
		}
		<-ctx.Done()
		return "", ctx.Err()
	})}
	started := time.Now()
	err := waitWorkspaceOwnerNoChild(context.Background(), owner, 500*time.Millisecond)
	elapsed := time.Since(started)
	var exitErr ExitError
	if calls != 2 || !AsExitError(err, &exitErr) || exitErr.Code != 7 || elapsed < 400*time.Millisecond || elapsed >= 1500*time.Millisecond {
		t.Fatalf("calls=%d err=%v elapsed=%s want 2 calls, exit 7, and about 500ms", calls, err, elapsed)
	}
}

func TestWorkspaceOwnerNativeWindowsProtocolProbesBeforeSingleDelivery(t *testing.T) {
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
	requireWorkspaceOwnerSSHProbe(t, dir, 1, "2222")
	requireWorkspaceOwnerSSHProbe(t, dir, 2, "22")
	command, input := readWorkspaceOwnerSSHCall(t, dir, 3)
	if command != windowsPowerShellStdinScriptCommand(len([]byte(input))) || !strings.Contains(input, "$action = 'acquire'") {
		t.Fatalf("delivery command=%q", command)
	}
}

func TestWorkspaceOwnerNativeWindowsAmbiguousStageCleansExactWitness(t *testing.T) {
	dir := installWorkspaceOwnerRecordingSSH(t)
	t.Setenv("CRABBOX_OWNER_SSH_FAIL_FIRST_DELIVERY", "1")
	target := SSHTarget{User: "crabbox", Host: "127.0.0.1", Port: "22", FallbackPorts: []string{"2222"}, TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	owner := &workspaceOwner{target: target, key: workspaceOwnerKey("cbx_windows_ambiguous_stage"), token: strings.Repeat("8", 64)}
	ctx := contextWithWorkspaceOwner(context.Background(), owner)
	_, err := prepareWorkspaceOwnerRemote(ctx, target, "Write-Output ambiguous-stage", nil)
	if err == nil || exitCode(err) != 7 || !strings.Contains(err.Error(), "stage native Windows workspace witness") {
		t.Fatalf("ambiguous stage err=%v", err)
	}

	requireWorkspaceOwnerSSHProbe(t, dir, 1, "22")
	stageCommand, stagedScript := readWorkspaceOwnerSSHCall(t, dir, 2)
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
	requireWorkspaceOwnerSSHProbe(t, dir, 3, "22")
	cleanupCommand, cleanupInput := readWorkspaceOwnerSSHCall(t, dir, 4)
	if cleanupCommand != remoteWorkspaceOwnerWindowsCleanupWitnessCommand(name) || cleanupInput != "" {
		t.Fatalf("cleanup command=%q stdin=%q want exact witness=%q", cleanupCommand, cleanupInput, name)
	}
	count, readErr := os.ReadFile(filepath.Join(dir, "count"))
	if readErr != nil || string(count) != "4" {
		t.Fatalf("SSH call count=%q err=%v; want probes around one failed stage and one cleanup", count, readErr)
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
		mask string
		args []string
	}{
		{name: "bash", mask: "022", args: []string{"--noprofile", "--norc", "-c"}},
		{name: "bash", mask: "027", args: []string{"--noprofile", "--norc", "-c"}},
		{name: "bash", mask: "077", args: []string{"--noprofile", "--norc", "-c"}},
		{name: "zsh", mask: "027", args: []string{"-f", "-c"}},
		{name: "fish", mask: "077", args: []string{"--no-config", "-c"}},
	}
	for _, shell := range shells {
		t.Run(shell.name+"/umask-"+shell.mask, func(t *testing.T) {
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

			command := func(remote string) *exec.Cmd {
				args := append([]string{"-c", `umask "$1"; shift; exec "$@"`, "umask-test", shell.mask, shellPath}, shell.args...)
				return exec.Command("sh", append(args, remote)...)
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
			acquireCmd := command(acquireTransport)
			acquireCmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
			if out, err := acquireCmd.CombinedOutput(); err != nil || string(out) != "ACQUIRED" {
				t.Fatalf("owner acquisition failed through %s: out=%q err=%v", shell.name, out, err)
			}

			privateCheck := `private_paths=$(find ` + shellQuote(ownerRoot) + ` \( -type d ! -perm 700 -o -type f ! -perm 600 \) -print) || exit 99; [ -z "$private_paths" ] || { printf 'non-private control paths: %s\n' "$private_paths" >&2; exit 99; }`
			userDir := filepath.Join(workdir, "user-directory")
			failedFile := filepath.Join(workdir, "nonzero.txt")
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
			}) + " && " + privateCheck + "\nmkdir " + shellQuote(userDir) + " && cat > " + shellQuote(inputPath)
			transportCommand := remoteWorkspaceOwnerPOSIXWitness(key, token, payload, true)
			const transportLimit = 30_000
			if !strings.HasPrefix(transportCommand, "exec /bin/sh -c '") || strings.Contains(transportCommand, "\n") || strings.Count(transportCommand, "'") != 2 {
				t.Fatalf("workspace owner transport is raw login-shell input: %q", transportCommand[:min(len(transportCommand), 80)])
			}
			t.Logf("workspace owner transport bytes=%d margin=%d", len(transportCommand), transportLimit-len(transportCommand))
			if len(transportCommand) >= transportLimit {
				t.Fatalf("workspace owner transport is too large for a bounded Windows SSH command: %d bytes", len(transportCommand))
			}
			cmd := command(transportCommand)
			cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
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

			nonzero := remoteWorkspaceOwnerPOSIXWitness(key, token, privateCheck+"\nprintf failed > "+shellQuote(failedFile)+"; exit 23")
			nonzeroCmd := command(nonzero)
			nonzeroCmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
			if out, err := nonzeroCmd.CombinedOutput(); err == nil || exitCode(err) != 23 {
				t.Fatalf("nonzero child through %s: out=%q err=%v", shell.name, out, err)
			}
			mask, err := strconv.ParseUint(shell.mask, 8, 32)
			if err != nil {
				t.Fatal(err)
			}
			for path, base := range map[string]os.FileMode{inputPath: 0o666, failedFile: 0o666, userDir: 0o777} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				want := base &^ os.FileMode(mask)
				if info.Mode().Perm() != want {
					t.Errorf("caller umask %s: %s mode=%04o want=%04o", shell.mask, filepath.Base(path), info.Mode().Perm(), want)
				}
			}
			entries, err = os.ReadDir(ownerRoot)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.Contains(entry.Name(), ".launcher.") || strings.Contains(entry.Name(), ".run.") || strings.HasSuffix(entry.Name(), ".child") {
					t.Errorf("nonzero transport left temporary owner state %q", entry.Name())
				}
			}
			request.Action = workspaceOwnerRelease
			releaseCmd := command(remoteWorkspaceOwnerCommand(SSHTarget{TargetOS: targetLinux}, request))
			releaseCmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
			if out, err := releaseCmd.CombinedOutput(); err != nil || string(out) != "RELEASED" {
				t.Fatalf("owner release through %s: out=%q err=%v", shell.name, out, err)
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
	// Remove payload bytes, not just padding that permissive decoders can omit.
	transport := remoteWorkspaceOwnerPOSIXEncodedLauncher(key, token, encoded[:len(encoded)-4], len(script))
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
	for _, path := range []string{childPath, filepath.Join(home, ".crabbox", "workspace-owners", key+".run."+tokenA)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("nonzero witnessed command left run state %q: %v", path, err)
		}
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
	if err := os.WriteFile(childPath, []byte(strconv.Itoa(os.Getpid())+"\n"+identity+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenB))); err != nil || out != "CHILD" {
		t.Fatalf("live child acquire out=%q err=%v", out, err)
	}
	if err := os.WriteFile(childPath, []byte("999999999\nold identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerAcquire, tokenB))); err != nil || out != "RECOVERED" {
		t.Fatalf("stale recovery out=%q err=%v", out, err)
	}
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, remoteWorkspaceOwnerPOSIX(request(workspaceOwnerRelease, tokenB))); err != nil || out != "RELEASED" {
		t.Fatalf("release out=%q err=%v", out, err)
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
	req.Action = workspaceOwnerRelease
	if out, err := runWindowsPowerShellScript(t, remoteWorkspaceOwnerWindows(req)); err != nil || !strings.Contains(string(out), "RELEASED") {
		t.Fatalf("Windows release out=%q err=%v", out, err)
	}
}
