package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLegacyBashPrerequisiteProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "present"}[present], func(t *testing.T) {
			dir := t.TempDir()
			if present {
				if err := os.WriteFile(filepath.Join(dir, "bash"), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("/bin/sh", "-c", legacyBashProbe)
			cmd.Env = []string{"PATH=" + dir}
			out, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			err = legacyBashProbeResult(string(out))
			if present && err != nil {
				t.Fatal(err)
			}
			if !present && (err == nil || !strings.Contains(err.Error(), missingNativeRuntimePackDiagnostic)) {
				t.Fatalf("missing actionable diagnostic: %v", err)
			}
		})
	}
	if err := legacyBashProbeResult("127\n"); err == nil || strings.Contains(err.Error(), "CLI-only") {
		t.Fatalf("unrelated output classified as missing runtime: %v", err)
	}
}

func TestLegacyBashPreflightMissingPackBeforeWorkload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH fixture")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nfor arg do remote=$arg; done\nprintf '%s\\n' \"$remote\" >> \"$CRABBOX_PREREQUISITE_LOG\"\nprintf 'CBX_LEGACY_BASH_MISSING\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_PREREQUISITE_LOG", log)
	scope := &nativeRuntimeScope{}
	ctx := context.WithValue(t.Context(), nativeRuntimeScopeKey{}, scope)
	target := SSHTarget{Host: "example.test", Port: "22", TargetOS: targetLinux}
	_, err := runPOSIXPreflight(ctx, target, functionalPOSIXPreflightHelper(time.Second, "bash"), func(string) string {
		return "printf USER_WORKLOAD"
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), missingNativeRuntimePackDiagnostic) {
		t.Fatalf("missing actionable diagnostic: %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != legacyBashProbe+"\n" {
		t.Fatalf("expected only prerequisite dispatch, got %q", data)
	}
}

func TestLegacyBashPrerequisiteCachedRouteHonorsCancellation(t *testing.T) {
	target := SSHTarget{TargetOS: targetLinux}
	scope := &nativeRuntimeScope{legacyBashReady: map[nativeRuntimeInstallKey]bool{{"", nativeRuntimeRouteKey(target)}: true}}
	ctx := context.WithValue(t.Context(), nativeRuntimeScopeKey{}, scope)
	if err := requireLegacyBash(ctx, target, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := requireLegacyBash(ctx, target, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestLegacyBashStageSelection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		program wslStageProgram
		want    bool
	}{
		{"legacy", wslStageProgram{source: wslLinuxHelper, bootstrap: wslHelperBootstrap}, true},
		{"posix", wslStageProgram{source: "exit 0", bootstrap: wslPOSIXHelperBootstrap}, false},
		{"native", wslStageProgram{source: "exit 0", bootstrap: wslPOSIXHelperBootstrap, runtime: &remoteNativeRuntime{}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spool, err := newWSLStageSpoolWithProgram("printf fixture", nil, nil, 0, sshCommandLimit{}, tc.program)
			if err != nil {
				t.Fatal(err)
			}
			defer spool.close()
			if spool.legacyBash != tc.want {
				t.Fatalf("legacy Bash prerequisite=%v, want %v", spool.legacyBash, tc.want)
			}
		})
	}
}

func TestLegacyBashPrerequisiteLeaseLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX SSH fixture")
	}
	log := installWSL2ReadinessRecorder(t, "exit 0", "true")
	target := SSHTarget{Host: "example.test", Port: "22", TargetOS: targetLinux}
	scope := &nativeRuntimeScope{leases: make(map[string]bool)}
	ctx := context.WithValue(t.Context(), nativeRuntimeScopeKey{}, scope)
	leaseA, leaseB := runtimeLeaseContext(ctx, "lease-a"), runtimeLeaseContext(ctx, "lease-b")
	for _, current := range []context.Context{leaseA, leaseA, leaseB, leaseB} {
		if err := requireLegacyBash(current, target, ""); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(before), "ssh:") != 2 {
		t.Fatalf("expected one independent probe per lease, got %q", before)
	}
	if err := scope.finalizeLease(ctx, "lease-a", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := requireLegacyBash(leaseA, target, ""); err == nil || !strings.Contains(err.Error(), "finalized") {
		t.Fatalf("finalized cached lease accepted: %v", err)
	}
	if err := scope.finalizeLease(ctx, "lease-c", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := requireLegacyBash(runtimeLeaseContext(ctx, "lease-c"), target, ""); err == nil || !strings.Contains(err.Error(), "finalized") {
		t.Fatalf("finalized uncached lease accepted: %v", err)
	}
	canceled, cancel := context.WithCancel(runtimeLeaseContext(ctx, "lease-d"))
	cancel()
	if err := requireLegacyBash(canceled, target, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled uncached lease accepted: %v", err)
	}
	after, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("finalized or canceled lease probed: %q", after)
	}
}
