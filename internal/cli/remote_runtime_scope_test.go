package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

func testNativeRuntimeScope() *nativeRuntimeScope {
	s := newNativeRuntimeScope()
	// These lifecycle tests do not read packs or contact an SSH target.
	s.loadOnce.Do(func() {})
	s.install = func(_ context.Context, target SSHTarget, _ runtimeartifact.Source) (*remoteNativeRuntime, error) {
		return &remoteNativeRuntime{target: target, path: "/tmp/example-runtime/crabbox"}, nil
	}
	s.remove = func(context.Context, *remoteNativeRuntime) error { return nil }
	return s
}

func runtimeLeaseContext(ctx context.Context, lease string) context.Context {
	return context.WithValue(ctx, nativeRuntimeLeaseKey{}, lease)
}

func TestNativeRuntimeScopeSharesInstallation(t *testing.T) {
	s := testNativeRuntimeScope()
	var installs atomic.Int32
	var removals atomic.Int32
	started, proceed := make(chan struct{}), make(chan struct{})
	s.install = func(_ context.Context, target SSHTarget, _ runtimeartifact.Source) (*remoteNativeRuntime, error) {
		if installs.Add(1) == 1 {
			close(started)
		}
		<-proceed
		return &remoteNativeRuntime{target: target}, nil
	}
	s.remove = func(context.Context, *remoteNativeRuntime) error { removals.Add(1); return nil }
	ctx := runtimeLeaseContext(t.Context(), "lease-a")
	target := SSHTarget{Host: "example.invalid", TargetOS: targetLinux}
	const callers = 8
	var results [callers]*remoteNativeRuntime
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			var err error
			results[i], err = s.ensure(ctx, target)
			if err != nil {
				t.Error(err)
			}
		})
	}
	<-started
	close(proceed)
	wg.Wait()
	for _, result := range results {
		if result == nil || result != results[0] {
			t.Fatal("callers did not share the installed handle")
		}
	}
	if installs.Load() != 1 || removals.Load() != 0 {
		t.Fatalf("installs=%d removals=%d", installs.Load(), removals.Load())
	}
	if err := s.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(ctx); err != nil || removals.Load() != 1 {
		t.Fatalf("duplicate finalization: removals=%d err=%v", removals.Load(), err)
	}
	if _, err := s.ensure(ctx, target); err == nil {
		t.Fatal("admitted a command after finalization")
	}
}

func TestNativeRuntimeScopeCanceledWaiterDoesNotReinstall(t *testing.T) {
	s := testNativeRuntimeScope()
	started, proceed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.install = func(context.Context, SSHTarget, runtimeartifact.Source) (*remoteNativeRuntime, error) {
		close(started)
		<-proceed
		return &remoteNativeRuntime{}, nil
	}
	ctx := runtimeLeaseContext(t.Context(), "lease-a")
	go func() {
		defer close(done)
		if _, err := s.ensure(ctx, SSHTarget{}); err != nil {
			t.Error(err)
		}
	}()
	<-started
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ensure(canceled, SSHTarget{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter did not cancel: %v", err)
	}
	close(proceed)
	<-done
	if _, err := s.ensure(context.WithoutCancel(canceled), SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNativeRuntimeScopeCachesFailure(t *testing.T) {
	s := testNativeRuntimeScope()
	want := errors.New("fixture installation failed")
	calls := 0
	s.install = func(context.Context, SSHTarget, runtimeartifact.Source) (*remoteNativeRuntime, error) {
		calls++
		return nil, want
	}
	for range 2 {
		if _, err := s.ensure(t.Context(), SSHTarget{}); !errors.Is(err, want) {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("retried installation %d times", calls)
	}
}

func TestNativeRuntimeScopeSeparatesLeaseAndRoute(t *testing.T) {
	s := testNativeRuntimeScope()
	ctx := runtimeLeaseContext(t.Context(), "lease-a")
	target := SSHTarget{Host: "example.invalid", User: "alice", Port: "22", TargetOS: targetLinux}
	original, err := s.ensure(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	target.NoControlMaster, target.FallbackPorts = true, []string{"2222"}
	same, err := s.ensure(ctx, target)
	if err != nil || same != original {
		t.Fatalf("connection options changed execution identity: %v", err)
	}
	replacement, err := s.ensure(runtimeLeaseContext(ctx, "lease-b"), target)
	if err != nil || replacement == original {
		t.Fatalf("replacement reused old lease runtime: %v", err)
	}
	target.ChildEnv = map[string]string{"EXAMPLE_TRANSPORT_OPTION": "synthetic"}
	changed, err := s.ensure(ctx, target)
	if err != nil || changed == original {
		t.Fatalf("changed route reused runtime: %v", err)
	}
	target.ChildEnv["EXAMPLE_TRANSPORT_OPTION"] = "new value"
	if changed.target.ChildEnv["EXAMPLE_TRANSPORT_OPTION"] != "synthetic" {
		t.Fatal("installed handle retained a mutable route map")
	}
	if err := s.finish(ctx); err != nil {
		t.Fatal(err)
	}
}

type runtimeReleasePolicy struct {
	SSHLeaseBackend
	preserves bool
}

func (p runtimeReleasePolicy) PreservesSSHWorkspaceAfterRelease() bool { return p.preserves }

func TestNativeRuntimeScopeLeaseDisposition(t *testing.T) {
	for _, tc := range []struct {
		name       string
		terminal   bool
		preserves  bool
		releaseErr error
		retained   bool
		remove     bool
	}{
		{name: "terminal_handoff", terminal: true},
		{name: "terminal_with_observation_error", terminal: true, releaseErr: errors.New("observation failed")},
		{name: "preserved_until_owner_close", terminal: true, preserves: true, remove: true},
		{name: "static_workspace_nonterminal_release", preserves: true, remove: true},
		{name: "preserved_release_failed", preserves: true, releaseErr: errors.New("release rejected"), retained: true},
		{name: "release_failed", releaseErr: errors.New("release rejected"), retained: true},
		{name: "release_unconfirmed", retained: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testNativeRuntimeScope()
			removed := 0
			s.remove = func(context.Context, *remoteNativeRuntime) error { removed++; return nil }
			ctx := runtimeLeaseContext(t.Context(), "lease-a")
			if _, err := s.ensure(ctx, SSHTarget{}); err != nil {
				t.Fatal(err)
			}
			err := s.afterRelease(ctx, "lease-a", runtimeReleasePolicy{preserves: tc.preserves}, ReleaseLeaseOutcome{Terminal: tc.terminal}, tc.releaseErr)
			if tc.retained != (err != nil) || tc.retained && !strings.Contains(err.Error(), "/tmp/example-runtime/crabbox") {
				t.Fatalf("retention result: %v", err)
			}
			if removed != 0 {
				t.Fatal("removed runtime before late owner finalization")
			}
			if err := s.finish(ctx); err != nil {
				t.Fatal(err)
			}
			if (removed == 1) != tc.remove {
				t.Fatalf("removals=%d", removed)
			}
		})
	}
}

func TestNativeRuntimeScopeRetainsUnretiredStage(t *testing.T) {
	for _, handoff := range []bool{false, true} {
		s := testNativeRuntimeScope()
		s.remove = func(context.Context, *remoteNativeRuntime) error { t.Error("removed retained runtime"); return nil }
		ctx := runtimeLeaseContext(t.Context(), "lease-a")
		runtime, err := s.ensure(ctx, SSHTarget{})
		if err != nil {
			t.Fatal(err)
		}
		s.retain(runtime, errors.New("command stage retirement unconfirmed"))
		err = s.finalizeLease(ctx, "lease-a", handoff, nil)
		if err == nil || !strings.Contains(err.Error(), runtime.path) || handoff && !strings.Contains(err.Error(), "file removal not verified") {
			t.Fatalf("missing retention/handoff locator: %v", err)
		}
	}
}

func TestNativeRuntimeScopePoolReturnRequiresCleanup(t *testing.T) {
	for _, fails := range []bool{false, true} {
		s := testNativeRuntimeScope()
		removed, returned := false, false
		s.remove = func(context.Context, *remoteNativeRuntime) error {
			removed = true
			if fails {
				return errors.New("fixture cleanup failed")
			}
			return nil
		}
		ctx := runtimeLeaseContext(t.Context(), "lease-a")
		if _, err := s.ensure(ctx, SSHTarget{}); err != nil {
			t.Fatal(err)
		}
		var owner *workspaceOwner
		err := returnReadyPoolAfterWorkspaceOwner(ctx, &owner, func(ctx context.Context) error {
			if err := s.finalizeLease(ctx, "lease-a", false, nil); err != nil {
				return err
			}
			returned = true
			return nil
		})
		if !removed || returned == fails || (err != nil) != fails {
			t.Fatalf("fails=%t removed=%t returned=%t err=%v", fails, removed, returned, err)
		}
	}
}

func TestNativeRuntimeAdmissionPreparesOutsideTransport(t *testing.T) {
	s := testNativeRuntimeScope()
	s.manifest = "selected fixture"
	ctx := context.WithValue(runtimeLeaseContext(t.Context(), "lease-a"), nativeRuntimeScopeKey{}, s)
	target := SSHTarget{Host: "example.invalid", Port: "2222", FallbackPorts: []string{"22"}, TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	var events []string
	s.ready = func(_ context.Context, target *SSHTarget, _ io.Writer) error {
		events = append(events, "ready")
		target.recordPreparedEndpoint("22")
		return nil
	}
	s.install = func(ctx context.Context, target SSHTarget, _ runtimeartifact.Source) (*remoteNativeRuntime, error) {
		if runtime, _ := admittedNativeRuntime(ctx, target); runtime != nil {
			t.Fatal("installation inherited an admitted handle")
		}
		events = append(events, "install")
		runtime := nativeWSLStageTestRuntime("/tmp/runtime/crabbox")
		runtime.target = target
		return runtime, nil
	}
	// A selected artifact alone must not change provider bootstrap commands.
	legacy, err := prepareSSHTransport(ctx, target, "true", nil, 0, sshCommandLimit{})
	if err != nil || legacy.stage.nativeRuntime != nil || len(events) != 0 {
		t.Fatalf("bootstrap activated native transport: %v %v", err, events)
	}
	if err := legacy.close(); err != nil {
		t.Fatal(err)
	}
	ctx, err = s.prepareCommandRuntime(ctx, &target, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ctx, err = s.prepareCommandRuntime(ctx, &target, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		transport, err := prepareSSHTransport(ctx, target, "true", nil, 0, sshCommandLimit{execution: time.Second, control: true})
		if err != nil || transport.stage.nativeRuntime == nil {
			t.Fatalf("native transport not selected: %v", err)
		}
		if err := transport.close(); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(events, ",") != "ready,install,ready,ready" {
		t.Fatalf("unexpected preparation: %v", events)
	}
	changed := target
	changed.Host = "other.invalid"
	if _, err := prepareSSHTransport(ctx, changed, "true", nil, 0, sshCommandLimit{}); !errors.Is(err, errNativeRuntimeUnprepared) {
		t.Fatalf("changed route borrowed runtime: %v", err)
	}
	if _, err := prepareSSHTransport(runtimeLeaseContext(ctx, "lease-b"), target, "true", nil, 0, sshCommandLimit{}); err == nil {
		t.Fatal("replacement borrowed old admission")
	}
	bootstrapCtx := contextWithoutNativeRuntimeAdmission(runtimeLeaseContext(ctx, ""))
	if native, err := admittedNativeRuntime(bootstrapCtx, target); native != nil || err != nil {
		t.Fatalf("cleared admission still selected: %v", err)
	}
	if err := s.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSSHTransport(ctx, target, "true", nil, 0, sshCommandLimit{}); err == nil {
		t.Fatal("finalized lease remained admitted")
	}
}

func TestNativeRuntimeAdmissionStopsBeforeInstallOnCancellation(t *testing.T) {
	for _, cancelDuringReady := range []bool{false, true} {
		s := testNativeRuntimeScope()
		s.manifest = "selected fixture"
		ctx, cancel := context.WithCancel(context.WithValue(t.Context(), nativeRuntimeScopeKey{}, s))
		target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
		ready := false
		s.ready = func(context.Context, *SSHTarget, io.Writer) error { ready = true; cancel(); return nil }
		s.install = func(context.Context, SSHTarget, runtimeartifact.Source) (*remoteNativeRuntime, error) {
			t.Error("installed after cancellation")
			return nil, nil
		}
		if !cancelDuringReady {
			cancel()
		}
		if _, err := s.prepareCommandRuntime(ctx, &target, io.Discard); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
		cancel()
		if ready != cancelDuringReady {
			t.Fatalf("ready=%t", ready)
		}
	}
}

func TestNativeRuntimeAdmissionRediscoversFallbackBeforeReusingInstallation(t *testing.T) {
	s := testNativeRuntimeScope()
	s.manifest = "selected fixture"
	ctx := context.WithValue(runtimeLeaseContext(t.Context(), "lease-a"), nativeRuntimeScopeKey{}, s)
	target := SSHTarget{Host: "example.invalid", Port: "2222", FallbackPorts: []string{"22"}, TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	healthyPort := "2222"
	var events []string
	s.ready = func(_ context.Context, target *SSHTarget, _ io.Writer) error {
		events = append(events, "ready:"+healthyPort)
		target.recordPreparedEndpoint(healthyPort)
		return nil
	}
	s.install = func(_ context.Context, target SSHTarget, _ runtimeartifact.Source) (*remoteNativeRuntime, error) {
		events = append(events, "install:"+target.Port)
		runtime := nativeWSLStageTestRuntime("/tmp/runtime/crabbox")
		runtime.target = target
		return runtime, nil
	}
	var err error
	ctx, err = s.prepareCommandRuntime(ctx, &target, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// The admitted endpoint disappears; discovery now selects its healthy fallback.
	healthyPort = "22"
	for range 2 {
		ctx, err = s.prepareCommandRuntime(ctx, &target, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		transport, err := prepareSSHTransport(ctx, target, "true", nil, 0, sshCommandLimit{execution: time.Second, control: true})
		if err != nil {
			t.Fatal(err)
		}
		if target.Port != healthyPort || transport.stage.nativeRuntime == nil || transport.stage.nativeRuntime.target.Port != healthyPort {
			t.Fatalf("fallback did not receive its own admitted runtime: target=%s", target.Port)
		}
		if err := transport.close(); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(events, ","); got != "ready:2222,install:2222,ready:22,install:22,ready:22" {
		t.Fatalf("unexpected route preparation: %s", got)
	}
}

func TestNativeRuntimeAdmissionRejectsStoppedRoutesBeforeReadiness(t *testing.T) {
	for _, reason := range []string{"finalized", "canceled", "installation failed"} {
		t.Run(reason, func(t *testing.T) {
			s := testNativeRuntimeScope()
			s.manifest = "selected fixture"
			ctx := context.WithValue(runtimeLeaseContext(t.Context(), "lease-a"), nativeRuntimeScopeKey{}, s)
			target := SSHTarget{Host: "example.invalid", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
			installationErr := errors.New("fixture installation failed")
			switch reason {
			case "finalized":
				if err := s.finalizeLease(ctx, "lease-a", true, nil); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "installation failed":
				s.install = func(context.Context, SSHTarget, runtimeartifact.Source) (*remoteNativeRuntime, error) {
					return nil, installationErr
				}
				if _, err := s.ensure(ctx, target); !errors.Is(err, installationErr) {
					t.Fatalf("fixture installation: %v", err)
				}
			}
			s.ready = func(context.Context, *SSHTarget, io.Writer) error {
				t.Fatal("stopped route attempted transport readiness")
				return nil
			}
			_, err := s.prepareCommandRuntime(ctx, &target, io.Discard)
			if err == nil {
				t.Fatal("stopped route was admitted")
			}
			if reason == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if reason == "installation failed" && !errors.Is(err, installationErr) {
				t.Fatalf("installation error lost: %v", err)
			}
		})
	}
}

func TestNativeRuntimeScopeDiscoveryFailsBeforeGuestAccess(t *testing.T) {
	for _, contents := range []string{"missing manifest", "malformed", "mismatched"} {
		t.Run(contents, func(t *testing.T) {
			dir := t.TempDir()
			controller := filepath.Join(dir, "crabbox")
			if err := os.WriteFile(controller, []byte("fixture controller"), 0600); err != nil {
				t.Fatal(err)
			}
			pack := filepath.Join(dir, "crabbox-runtime")
			if err := os.Mkdir(pack, 0700); err != nil {
				t.Fatal(err)
			}
			if contents != "missing manifest" {
				data := []byte("malformed")
				if contents == "mismatched" {
					data = []byte(`{"schemaVersion":1,"protocolVersion":"CBX-REMOTE-1","controllerSha256":"` + strings.Repeat("0", 64) + `","artifacts":[{"os":"linux","arch":"amd64","path":"linux-amd64","size":1,"sha256":"` + strings.Repeat("0", 64) + `"}]}`)
				}
				if err := os.WriteFile(filepath.Join(pack, "manifest.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			s := newNativeRuntimeScopeForExecutable(controller, "", nil)
			s.ready = func(context.Context, *SSHTarget, io.Writer) error { t.Fatal("invalid pack accessed guest"); return nil }
			s.install = func(context.Context, SSHTarget, runtimeartifact.Source) (*remoteNativeRuntime, error) {
				t.Fatal("invalid pack installed")
				return nil, nil
			}
			ctx := context.WithValue(t.Context(), nativeRuntimeScopeKey{}, s)
			target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
			if _, err := s.prepareCommandRuntime(ctx, &target, io.Discard); err == nil {
				t.Fatal("invalid discovered pack fell back")
			}
		})
	}
}

func TestNativeRuntimeScopeMissingPackPreservesCLIOnly(t *testing.T) {
	dir := t.TempDir()
	controller := filepath.Join(dir, "crabbox")
	if err := os.WriteFile(controller, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	s := newNativeRuntimeScopeForExecutable(controller, "", nil)
	s.ready = func(context.Context, *SSHTarget, io.Writer) error {
		t.Fatal("CLI-only scope accessed guest")
		return nil
	}
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	if _, err := s.prepareCommandRuntime(t.Context(), &target, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ensure(t.Context(), target); err == nil || !strings.Contains(err.Error(), "CLI-only") {
		t.Fatalf("missing recovery: %v", err)
	}
}

func TestNativeRuntimePreflightRejectsLocalFailureBeforeTransport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake SSH fixture")
	}
	target := SSHTarget{Host: "example.test", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	logPath := installWSL2ReadinessRecorder(t, "exit 0", "true")
	for _, state := range []string{"invalid pack", "canceled", "finalized"} {
		t.Run(state, func(t *testing.T) {
			s := newNativeRuntimeScope()
			s.manifest = ""
			invalid := errors.New("fixture incomplete runtime pack")
			s.discoveryErr = invalid
			ctx := context.WithValue(runtimeLeaseContext(t.Context(), "lease-a"), nativeRuntimeScopeKey{}, s)
			if state == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if state == "finalized" {
				if err := s.finalizeLease(ctx, "lease-a", true, nil); err != nil {
					t.Fatal(err)
				}
			}
			check := func(err error) {
				t.Helper()
				if err == nil {
					t.Fatal("preflight accepted unavailable runtime")
				}
				if state == "invalid pack" && !errors.Is(err, invalid) {
					t.Fatalf("discovery error lost: %v", err)
				}
				if state == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
				if state == "finalized" && !strings.Contains(err.Error(), "finalized") {
					t.Fatalf("finalization lost: %v", err)
				}
			}
			_, err := runWSLFunctionalPreflight(ctx, target, "/tmp", nil, nil)
			check(err)
			linux := target
			linux.TargetOS = targetLinux
			linux.WindowsMode = ""
			_, err = runPOSIXPreflight(ctx, linux, functionalPOSIXPreflightHelper(time.Second, "/bin/sh"), func(string) string { return "true" }, io.Discard, io.Discard)
			check(err)
			if probeSSHTransport(ctx, &target, time.Second) {
				t.Fatal("invalid scope probed guest")
			}
		})
	}
	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("local rejection accessed guest: %q", data)
	}
}

func TestFilesystemRuntimeScopeOwnsCapabilityAndLifecycle(t *testing.T) {
	scope, err := newFilesystemRuntimeScope(nil)
	if err != nil {
		t.Fatal(err)
	}
	if scope.required.Capability != runtimeartifact.Filesystem || scope.required.ProtocolVersion != "1" || scope.required.BuildID == "" {
		t.Fatalf("filesystem requirement = %+v", scope.required)
	}
	// Mock only installation and removal: no pack, compilation, or guest access.
	scope.loadOnce.Do(func() {})
	var removed int
	scope.install = func(_ context.Context, target SSHTarget, _ runtimeartifact.Source) (*remoteNativeRuntime, error) {
		return &remoteNativeRuntime{target: target, path: "/tmp/crabbox-runtime-test/crabbox", identity: runtimeartifact.Identity{
			Target: runtimeartifact.Target{OS: "darwin", Arch: "arm64"}, Capability: runtimeartifact.Filesystem,
			ProtocolVersion: "1", BuildID: scope.required.BuildID,
		}}, nil
	}
	scope.remove = func(context.Context, *remoteNativeRuntime) error { removed++; return nil }
	ctx := runtimeLeaseContext(t.Context(), "filesystem-lease")
	target := SSHTarget{TargetOS: targetMacOS}
	client, err := scope.filesystemClient(ctx, target, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if client.Identity.OS != "darwin" || client.Identity.Arch != "arm64" || client.Identity.BuildID != scope.required.BuildID || client.Identity.Protocol != 1 {
		t.Fatalf("client identity = %+v", client.Identity)
	}
	if err := scope.finish(ctx); err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d installations", removed)
	}
	if err := client.Transport(ctx, nil, io.Discard); err == nil {
		t.Fatal("client reused finalized installation")
	}
	supervisor := testNativeRuntimeScope()
	if _, err := supervisor.filesystemClient(ctx, target, io.Discard); err == nil {
		t.Fatal("supervisor accepted filesystem client")
	}
}

func TestFilesystemSourceErrorPrecedesGuestAccess(t *testing.T) {
	scope, err := newFilesystemRuntimeScope(nil)
	if err != nil {
		t.Fatal(err)
	}
	scope.discoveryErr = nil
	scope.manifest = filepath.Join(t.TempDir(), "missing-manifest.json")
	scope.install = func(context.Context, SSHTarget, runtimeartifact.Source) (*remoteNativeRuntime, error) {
		t.Fatal("guest installer called after local source failure")
		return nil, nil
	}
	if _, err := scope.ensure(t.Context(), SSHTarget{TargetOS: targetLinux}); err == nil {
		t.Fatal("missing explicit pack accepted")
	}
}

func TestFilesystemCanceledSpoolDoesNotRetainInstallation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	unknown, err := runFilesystemCommand(ctx, SSHTarget{}, func(int64) string {
		called = true
		return "unused"
	}, strings.NewReader("ordinary input"), io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) || unknown || called {
		t.Fatalf("canceled spool: error=%v unknown=%v dispatched=%v", err, unknown, called)
	}
}

func TestPOSIXRuntimeCapabilityTargets(t *testing.T) {
	supervisor := runtimeartifact.Requirement{Capability: runtimeartifact.Supervisor, ProtocolVersion: "CBX-REMOTE-1"}
	filesystem := runtimeartifact.Requirement{Capability: runtimeartifact.Filesystem, ProtocolVersion: "1", BuildID: "source"}
	for _, test := range []struct {
		name     string
		target   SSHTarget
		required runtimeartifact.Requirement
		want     string
	}{
		{"supervisor linux", SSHTarget{TargetOS: targetLinux}, supervisor, "linux"},
		{"supervisor macOS rejected", SSHTarget{TargetOS: targetMacOS}, supervisor, ""},
		{"filesystem linux", SSHTarget{TargetOS: targetLinux}, filesystem, "linux"},
		{"filesystem macOS", SSHTarget{TargetOS: targetMacOS}, filesystem, "darwin"},
		{"filesystem WSL", SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, filesystem, "linux"},
		{"filesystem native Windows rejected", SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, filesystem, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := posixRuntimeOS(test.target, test.required)
			if got != test.want || (err != nil) != (test.want == "") {
				t.Fatalf("OS=%q err=%v", got, err)
			}
		})
	}
}
