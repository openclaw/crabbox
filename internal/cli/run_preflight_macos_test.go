package cli

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMacOSPreflightTiming(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	for _, tc := range []struct {
		real string
		code string
		want time.Duration
	}{
		{"0.00", "0", 10 * time.Millisecond},
		{"1.43", "7", 1440 * time.Millisecond},
		{"4.99", "127", 5 * time.Second},
	} {
		elapsed, code, err := parseMacOSPreflightTiming("real "+tc.real+"\nuser 0.00\nsys 0.00\nCBX-MACOS-TIME-1 "+nonce+" "+tc.code+"\n", nonce)
		if err != nil || elapsed != tc.want || strconv.Itoa(code) != tc.code {
			t.Fatalf("%+v: elapsed=%v code=%d err=%v", tc, elapsed, code, err)
		}
	}
}

func TestMacOSPreflightSelection(t *testing.T) {
	mac := SSHTarget{TargetOS: targetMacOS}
	if !slices.Contains(preflightToolsForTarget(mac, nil), macOSPlatformPreflightTool) {
		t.Fatal("macOS default omitted platform snapshot")
	}
	for _, name := range []string{"swift", "xcodebuild", "brew"} {
		if slices.Contains(preflightToolsForTarget(mac, nil), name) || !reflect.DeepEqual(preflightToolsForTarget(mac, []string{name}), []string{name}) {
			t.Fatalf("%s must be a literal opt-in", name)
		}
	}
	for _, target := range []SSHTarget{{TargetOS: targetLinux}, {TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, {TargetOS: targetWindows, WindowsMode: windowsModeNormal}} {
		for _, tool := range preflightToolsForTarget(target, nil) {
			if isMacOSPreflightTool(tool) {
				t.Fatalf("macOS probe added to another target: %+v / %s", target, tool)
			}
		}
	}
	for _, tools := range [][]string{{"none"}, {}} {
		if len(preflightToolsForTarget(mac, tools)) != 0 {
			t.Fatalf("disabled probes were restored: %v", tools)
		}
	}
	if got := preflightToolsForTarget(mac, []string{"none", macOSPlatformPreflightTool}); !reflect.DeepEqual(got, []string{macOSPlatformPreflightTool}) {
		t.Fatalf("mixed none selection: %v", got)
	}
}

func TestMacOSPreflightValue(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"Apple Swift 6.0\nTarget: arm64-apple-macosx", "Apple Swift 6.0"},
		{"\nignored", "missing"},
		{"   ", "missing"},
		{strings.Repeat("x", 1000), strings.Repeat("x", 512)},
		{strings.Repeat("é", 1000), strings.Repeat("é", 512)},
	} {
		if got := macOSPreflightValue(tc.input); got != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
}

func TestMacOSPreflightBudgetAndDiagnostics(t *testing.T) {
	original := runMacOSPreflightProbe
	t.Cleanup(func() { runMacOSPreflightProbe = original })
	for _, exhausted := range []bool{false, true} {
		var budgets []time.Duration
		runMacOSPreflightProbe = func(_ context.Context, _ SSHTarget, workdir string, env map[string]string, envFiles []string, script string, budget time.Duration) (macOSPreflightProbeResult, error) {
			if workdir != "/work" || env["DEVELOPER_DIR"] != "/selected" || !reflect.DeepEqual(envFiles, []string{"child.env"}) {
				t.Fatal("probe lost workload context")
			}
			budgets = append(budgets, budget)
			if len(budgets) == 3 && !strings.Contains(script, "HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1") {
				t.Fatal("brew probe lacks child-only policy")
			}
			if exhausted {
				return macOSPreflightProbeResult{elapsed: budget}, nil
			}
			return macOSPreflightProbeResult{output: "Version 1\nsecond line", elapsed: min(budget, 4900*time.Millisecond), ready: true}, nil
		}
		var out bytes.Buffer
		err := printMacOSCapabilityPreflight(t.Context(), &out, SSHTarget{TargetOS: targetMacOS}, "/work", map[string]string{"DEVELOPER_DIR": "/selected"}, []string{"child.env"}, []string{"swift", "xcodebuild", "brew", macOSPlatformPreflightTool})
		if err != nil {
			t.Fatal(err)
		}
		want := []time.Duration{5 * time.Second, 5 * time.Second, 5 * time.Second}
		if exhausted {
			if !strings.Contains(out.String(), "macos_version=missing reason=budget-exhausted") {
				t.Fatal(out.String())
			}
		} else {
			want = append(want, 300*time.Millisecond)
			if !strings.Contains(out.String(), "brew=Version 1\n") || strings.Contains(out.String(), "second line") {
				t.Fatal(out.String())
			}
		}
		if !reflect.DeepEqual(budgets, want) {
			t.Fatalf("execution allowances %v, want %v", budgets, want)
		}
	}
	ownedFailure := errors.New("retirement failed")
	runMacOSPreflightProbe = func(context.Context, SSHTarget, string, map[string]string, []string, string, time.Duration) (macOSPreflightProbeResult, error) {
		return macOSPreflightProbeResult{}, ownedFailure
	}
	var out bytes.Buffer
	if err := printMacOSCapabilityPreflight(t.Context(), &out, SSHTarget{TargetOS: targetMacOS}, "/work", nil, nil, []string{"swift"}); !errors.Is(err, ownedFailure) || out.Len() != 0 {
		t.Fatalf("operational failure converted to a diagnostic: %v / %q", err, out.String())
	}
}

func TestMacOSPreflightExhaustedPlatformKeepsFields(t *testing.T) {
	original := runMacOSPreflightProbe
	t.Cleanup(func() { runMacOSPreflightProbe = original })
	calls := 0
	runMacOSPreflightProbe = func(_ context.Context, _ SSHTarget, _ string, _ map[string]string, _ []string, _ string, budget time.Duration) (macOSPreflightProbeResult, error) {
		calls++
		return macOSPreflightProbeResult{elapsed: budget}, nil
	}
	var out bytes.Buffer
	err := printMacOSCapabilityPreflight(t.Context(), &out, SSHTarget{TargetOS: targetMacOS}, "/work", nil, nil, []string{"swift", "xcodebuild", "brew", macOSPlatformPreflightTool})
	if err != nil || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	for _, key := range []string{"macos_version", "macos_build", "architecture", "developer_directory", "developer_tools"} {
		value := "missing"
		if key == "developer_tools" {
			value = "unavailable"
		}
		if !strings.Contains(out.String(), "remote preflight "+key+"="+value+" reason=budget-exhausted\n") {
			t.Fatalf("missing platform field %s: %s", key, out.String())
		}
	}
}

func TestMacOSPlatformPreflightNative(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native macOS snapshot")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := printMacOSPlatformPreflight(&out, func(_, command string) (macOSPreflightProbeResult, string, error) {
		value, err := exec.CommandContext(ctx, "/bin/bash", "-c", command).Output()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			return macOSPreflightProbeResult{}, "", err
		}
		return macOSPreflightProbeResult{output: string(value), ready: err == nil, completed: true}, "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, command, arg string }{
		{"macos_version", "/usr/bin/sw_vers", "-productVersion"},
		{"macos_build", "/usr/bin/sw_vers", "-buildVersion"},
		{"architecture", "/usr/bin/uname", "-m"},
	} {
		want, err := exec.CommandContext(ctx, tc.command, tc.arg).Output()
		if err != nil || !strings.Contains(out.String(), tc.key+"="+strings.TrimSpace(string(want))+"\n") {
			t.Fatalf("native %s: %s (%v)", tc.key, out.String(), err)
		}
	}
}

func TestMacOSPlatformPerCommandBudgetAndPartialResults(t *testing.T) {
	original := runMacOSPreflightProbe
	t.Cleanup(func() { runMacOSPreflightProbe = original })
	for _, timeout := range []bool{false, true} {
		calls := 0
		runMacOSPreflightProbe = func(_ context.Context, _ SSHTarget, _ string, _ map[string]string, _ []string, script string, budget time.Duration) (macOSPreflightProbeResult, error) {
			calls++
			if budget != macOSPreflightCommandTime {
				t.Fatalf("command %d lost execution time to earlier platform commands: %v", calls, budget)
			}
			if calls <= 4 {
				return macOSPreflightProbeResult{output: "observed", ready: true, completed: true, elapsed: 20 * time.Millisecond}, nil
			}
			if timeout && calls == 5 {
				return macOSPreflightProbeResult{elapsed: budget}, nil
			}
			// Two healthy commands can each need three seconds: they must
			// not compete for one five-second platform allowance.
			return macOSPreflightProbeResult{output: "Version 1", ready: true, completed: true, elapsed: 3 * time.Second}, nil
		}
		var out bytes.Buffer
		if err := printMacOSCapabilityPreflight(t.Context(), &out, SSHTarget{TargetOS: targetMacOS}, "/work", nil, nil, []string{macOSPlatformPreflightTool}); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"macos_version", "macos_build", "architecture", "developer_directory"} {
			if !strings.Contains(out.String(), key+"=observed\n") {
				t.Fatalf("completed field lost: %s", out.String())
			}
		}
		wantCalls, tools := 6, "xcode"
		if timeout {
			wantCalls, tools = 5, "unavailable"
		}
		if calls != wantCalls || !strings.Contains(out.String(), "developer_tools="+tools+"\n") {
			t.Fatalf("calls=%d output=%s", calls, out.String())
		}
	}
}

func TestMacOSPreflightWorkerNative(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Apple time output")
	}
	t.Setenv("LC_ALL", "POSIX")
	for _, tc := range []struct {
		name, script, output string
		code                 int
	}{
		{"success", "printf 'Version 1\\nsecond line\\n'", "Version 1\nsecond line\n", 0},
		{"nonzero stdout", "printf 'not a successful version\\n'; exit 7", "not a successful version\n", 7},
		{"empty", "exit 0", "", 0},
		{"child locale", "printf %s \"$LC_ALL\"", "POSIX", 0},
		{"child umask", "umask", "0027\n", 0},
		{"long stdout", "printf '%05000d' 0", strings.Repeat("0", 4096), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nonce := strings.Repeat("b", 32)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/bash", "-c", "umask 027\n"+macOSPreflightWorker(nonce, t.TempDir(), tc.script))
			var out, report bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &report
			if err := cmd.Run(); err != nil {
				t.Fatalf("worker: %v / %s", err, report.String())
			}
			value, elapsed, code, err := parseMacOSPreflightOutput(out.String(), nonce)
			if err != nil || code != tc.code || value != tc.output || elapsed <= 0 {
				t.Fatalf("output=%q code=%d elapsed=%v err=%v report=%q", value, code, elapsed, err, report.String())
			}
		})
	}
}
