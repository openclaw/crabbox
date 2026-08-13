package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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
	if !shouldAcquireWorkspaceOwner(true, nonExclusive) {
		t.Fatal("a successful non-exclusive acquisition must acquire the workspace owner")
	}
	exclusive := runEnvProfileTestBackend{}
	if shouldAcquireWorkspaceOwner(true, exclusive) {
		t.Fatal("newly acquired exclusive one-shot lease must bypass the workspace owner")
	}
	if !shouldAcquireWorkspaceOwner(false, exclusive) {
		t.Fatal("existing, pooled, and watch-reused leases must acquire the reuse owner")
	}
}

func TestWorkspaceOwnerContextWrapsEverySSHChild(t *testing.T) {
	owner := &workspaceOwner{target: SSHTarget{TargetOS: targetLinux}, key: strings.Repeat("a", 64), token: strings.Repeat("b", 64)}
	ctx := contextWithWorkspaceOwner(context.Background(), owner)
	if got, want := wrapWorkspaceOwnerRemote(ctx, "printf ok", false), remoteWorkspaceOwnerPOSIXWitness(owner.key, owner.token, "printf ok"); got != want {
		t.Fatalf("ordinary SSH child was not witnessed:\n%s", got)
	}
	if script := remoteWorkspaceOwnerPOSIXWitnessScript(owner.key, owner.token, "printf ok"); !strings.Contains(script, "child_identity=$(ps -o lstart=") {
		t.Fatalf("ordinary SSH child witness lost identity fencing:\n%s", script)
	}
	if got, want := wrapWorkspaceOwnerRemote(ctx, "cat", true), remoteWorkspaceOwnerPOSIXWitness(owner.key, owner.token, "cat", true); got != want {
		t.Fatalf("input SSH child did not preserve stdin:\n%s", got)
	}
	if script := remoteWorkspaceOwnerPOSIXWitnessScript(owner.key, owner.token, "cat", true); !strings.Contains(script, `cat >"$run_dir/input"`) || !strings.Contains(script, `<"$run_dir/input"`) {
		t.Fatalf("input SSH child witness lost stdin preservation:\n%s", script)
	}
	if got := wrapWorkspaceOwnerRemote(contextWithoutWorkspaceOwner(ctx), "printf raw", false); got != "printf raw" {
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
			if !shouldAcquireWorkspaceOwner(false, nil) {
				t.Fatalf("reused %s path bypassed workspace ownership", lifecycle)
			}
		})
	}
}

func TestWorkspaceOwnerSerializesStaticRunAndStandaloneActionsHydration(t *testing.T) {
	if !shouldAcquireWorkspaceOwner(true, testStaticSSHBackend{}) {
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

	windows := decodePowerShellCommand(t, remoteWorkspaceOwnerCommand(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, req))
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
	for _, want := range []string{"child_identity=$(ps -o lstart=", "owner_expiry=$(sed -n", "owner_expiry", "date +%s", "mv \"$child_tmp\" \"$child\"", "touch \"$start\"", "wait \"$child_pid\"", "rm -f \"$child\""} {
		if !strings.Contains(posixWitness, want) {
			t.Fatalf("POSIX child witness missing %q:\n%s", want, posixWitness)
		}
	}
	windowsWitness := decodePowerShellCommand(t, remoteWorkspaceOwnerWindowsWitness(key, token, "Write-Output ok"))
	for _, want := range []string{"Start-Process", "StartTime.ToUniversalTime().Ticks", "Read-Expiry", "(Read-Expiry) -le [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()", "Move-Item -LiteralPath $tmp -Destination $child", "$process.WaitForExit()", "Remove-Item -LiteralPath $child"} {
		if !strings.Contains(windowsWitness, want) {
			t.Fatalf("Windows child witness missing %q:\n%s", want, windowsWitness)
		}
	}
	windowsInputWitness := decodePowerShellCommand(t, remoteWorkspaceOwnerWindowsWitness(key, token, "Write-Output ok", true))
	for _, want := range []string{"[Console]::OpenStandardInput().CopyTo($inputFile)", "-RedirectStandardInput $inputPath", "[IO.FileShare]::None"} {
		if !strings.Contains(windowsInputWitness, want) {
			t.Fatalf("Windows input witness missing %q:\n%s", want, windowsInputWitness)
		}
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
	if out, err := runPOSIXWorkspaceOwnerScript(t, home, guardOwner.WrapBackgroundCommand(guardOwner.rsyncGuardPayload(filepath.Join(home, "guard-destination")))); err != nil || strings.TrimSpace(out) == "" {
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
	if out, err := runWindowsPowerShellScript(t, decodePowerShellCommand(t, remoteWorkspaceOwnerWindows(req))); err != nil || !strings.Contains(string(out), "ACQUIRED") {
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
	if out, err := runWindowsPowerShellScript(t, decodePowerShellCommand(t, remoteWorkspaceOwnerWindows(req))); err == nil || !strings.Contains(string(out), "EXPIRED") {
		t.Fatalf("late Windows renew out=%q err=%v", out, err)
	}
	req.Action = workspaceOwnerInspect
	if out, err := runWindowsPowerShellScript(t, decodePowerShellCommand(t, remoteWorkspaceOwnerWindows(req))); err == nil || !strings.Contains(string(out), "EXPIRED") {
		t.Fatalf("late Windows inspect out=%q err=%v", out, err)
	}
	if out, err := runWindowsPowerShellScript(t, decodePowerShellCommand(t, remoteWorkspaceOwnerWindowsWitness(key, token, "Write-Output late-child"))); err == nil || strings.Contains(string(out), "late-child") {
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
	if out, err := runWindowsPowerShellScript(t, decodePowerShellCommand(t, remoteWorkspaceOwnerWindows(req))); err != nil || !strings.Contains(string(out), "RECOVERED") {
		t.Fatalf("recover expired Windows generation out=%q err=%v", out, err)
	}
	if out, err := runWindowsPowerShellScript(t, decodePowerShellCommand(t, remoteWorkspaceOwnerWindowsWitness(key, token, "Write-Output child-ok"))); err != nil || !strings.Contains(string(out), "child-ok") {
		t.Fatalf("Windows witnessed child out=%q err=%v", out, err)
	}
	req.Action = workspaceOwnerRelease
	if out, err := runWindowsPowerShellScript(t, decodePowerShellCommand(t, remoteWorkspaceOwnerWindows(req))); err != nil || !strings.Contains(string(out), "RELEASED") {
		t.Fatalf("Windows release out=%q err=%v", out, err)
	}
}
