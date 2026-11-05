package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNativeLinuxReadinessScope(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX transport fixture")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	writeExecutable(t, filepath.Join(dir, "ssh"), "#!/bin/sh\nprintf 'call\\n' >> "+shellQuote(log)+"\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := testNativeRuntimeScope()
	s.manifest = "selected fixture"
	ctx := context.WithValue(runtimeLeaseContext(t.Context(), "lease-a"), nativeRuntimeScopeKey{}, s)
	target := SSHTarget{Host: "127.0.0.1", Port: startTCPReadinessFixture(t), TargetOS: targetLinux, ReadyCheck: "true", NoControlMaster: true}
	check := func(ctx context.Context) {
		t.Helper()
		if err := waitForSSHReady(ctx, &target, io.Discard, "fixture", time.Second); err != nil {
			t.Fatal(err)
		}
	}
	check(ctx)
	check(ctx)
	owner := &workspaceOwner{}
	owned := contextWithWorkspaceOwner(ctx, owner)
	check(owned)
	check(owned)
	target.ReadyCheck = "exit 0"
	check(owned)
	check(runtimeLeaseContext(owned, "lease-b"))
	owner.renewErr = errors.New("owner lost")
	if err := waitForSSHReady(owned, &target, io.Discard, "fixture", time.Second); !errors.Is(err, owner.renewErr) {
		t.Fatalf("cached readiness concealed lost owner: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := waitForSSHReady(canceled, &target, io.Discard, "fixture", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached readiness concealed cancellation: %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil || strings.Count(string(data), "call\n") != 4 {
		t.Fatalf("readiness must run once per lease, route and owner: %q %v", data, err)
	}
}

func TestNativeLinuxPreparationReusesInstallation(t *testing.T) {
	s := testNativeRuntimeScope()
	s.manifest = "selected fixture"
	calls := 0
	s.ready = func(context.Context, *SSHTarget, io.Writer) error { calls++; return nil }
	ctx := context.WithValue(runtimeLeaseContext(t.Context(), "lease-a"), nativeRuntimeScopeKey{}, s)
	target := SSHTarget{Host: "fixture.invalid", Port: "22", TargetOS: targetLinux}
	for range 3 {
		var err error
		ctx, err = s.prepareCommandRuntime(ctx, &target, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || target.NoControlMaster {
		t.Fatalf("preparation: readiness=%d dedicated metadata=%t", calls, target.NoControlMaster)
	}
	target.Host = "other.invalid"
	if _, err := s.prepareCommandRuntime(ctx, &target, io.Discard); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("changed route reused readiness: %d", calls)
	}
}
