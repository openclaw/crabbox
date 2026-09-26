package ssh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStaticSSHArchitectureLocalMacProbe(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires local macOS system queries")
	}
	ctx := t.Context()
	output, err := exec.CommandContext(ctx, "/bin/sh", "-c", macArchitectureProbe).Output()
	if err != nil {
		t.Fatalf("local macOS probe: %v (context: %v)", err, ctx.Err())
	}
	observation, err := parseArchitectureObservation(string(output), true)
	if err != nil {
		t.Fatal(err)
	}
	if !supportedArchitecture(observation.architecture) || !supportedArchitecture(observation.host) || observation.architecture != observation.process {
		t.Fatalf("incomplete local macOS architecture evidence: %+v", observation)
	}
	switch observation.translated {
	case "false":
		if observation.host != observation.process {
			t.Fatalf("native probe contradicts hardware: %+v", observation)
		}
	case "true":
		if observation.host != "arm64" || observation.process != "amd64" {
			t.Fatalf("Rosetta evidence contradicts host/process: %+v", observation)
		}
	default:
		t.Fatalf("local macOS translation query unavailable: %+v", observation)
	}
	t.Logf("local macOS evidence: %s", output)
}

func TestStaticSSHArchitectureLocalPOSIXProbe(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires local POSIX system queries")
	}
	ctx := t.Context()
	output, err := exec.CommandContext(ctx, "/bin/sh", "-c", posixArchitectureProbe).Output()
	if err != nil {
		t.Fatalf("local POSIX probe: %v (context: %v)", err, ctx.Err())
	}
	observation, err := parseArchitectureObservation(string(output), false)
	if err != nil {
		t.Fatal(err)
	}
	if !supportedArchitecture(observation.architecture) || observation.host != "" || observation.process != "" || observation.translated != "" {
		t.Fatalf("invalid POSIX execution-environment evidence: %+v", observation)
	}
	t.Logf("local POSIX evidence: %s", output)
}

func TestStaticSSHArchitectureLocalWindowsPowerShell51Probe(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires native Windows PowerShell Desktop 5.1")
	}
	systemRoot := os.Getenv("SystemRoot")
	if !filepath.IsAbs(systemRoot) {
		t.Fatal("SystemRoot must identify the Windows installation")
	}
	powershell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	// This smoke checks native evidence, not cold compiler/startup latency.
	// Production deadlines are asserted by TestStaticSSHArchitectureBudgetOwnership.
	ctx := t.Context()
	// Identify the same interpreter that evaluates the unchanged production script, with one cold start.
	command := `[Console]::WriteLine($PSVersionTable.PSEdition + '|' + $PSVersionTable.PSVersion.ToString())` + "\n" + windowsArchitectureProbe
	started := time.Now()
	combinedOutput, err := exec.CommandContext(ctx, powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command).Output()
	if err != nil {
		t.Fatalf("local Windows architecture probe: %v (elapsed: %s, context: %v) %s", err, time.Since(started), ctx.Err(), windowsArchitectureProbeFailureOutput(combinedOutput, err))
	}
	versionOutput, output, ok := strings.Cut(string(combinedOutput), "\n")
	if !ok {
		t.Fatalf("missing Windows PowerShell version evidence: %q", combinedOutput)
	}
	engine := strings.Split(strings.TrimSpace(versionOutput), "|")
	if len(engine) != 2 {
		t.Fatalf("unexpected Windows PowerShell version evidence: %q", versionOutput)
	}
	version := strings.Split(engine[1], ".")
	if engine[0] != "Desktop" || len(version) < 2 || version[0] != "5" || version[1] != "1" {
		t.Fatalf("requires Windows PowerShell Desktop 5.1, got %q", versionOutput)
	}
	t.Logf("Windows PowerShell edition=%s version=%s", engine[0], engine[1])

	if len(output) > architectureProbeLimit {
		t.Fatalf("local Windows architecture evidence exceeds %d bytes", architectureProbeLimit)
	}
	observation, err := parseArchitectureObservation(output, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("local Windows evidence: %s", output)
	if !supportedArchitecture(observation.architecture) || !supportedArchitecture(observation.host) ||
		observation.architecture != observation.process || observation.host != observation.process || observation.translated != "false" {
		t.Fatalf("incomplete or non-native local Windows architecture evidence: %+v", observation)
	}
}

func windowsArchitectureProbeFailureOutput(output []byte, err error) string {
	var stderr []byte
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr != nil {
		stderr = exitErr.Stderr
	}
	return fmt.Sprintf("stdout_prefix=%q stdout_bytes=%d stdout_truncated=%t stderr_prefix=%q stderr_available_bytes=%d stderr_truncated=%t",
		output[:min(len(output), architectureProbeLimit)], len(output), len(output) > architectureProbeLimit,
		stderr[:min(len(stderr), architectureProbeLimit)], len(stderr), len(stderr) > architectureProbeLimit)
}

func TestWindowsArchitectureProbeFailureOutput(t *testing.T) {
	t.Run("available output", func(t *testing.T) {
		err := &exec.ExitError{Stderr: []byte("compiler diagnostic")}
		got := windowsArchitectureProbeFailureOutput([]byte("Desktop|5.1\n"), err)
		want := `stdout_prefix="Desktop|5.1\n" stdout_bytes=12 stdout_truncated=false stderr_prefix="compiler diagnostic" stderr_available_bytes=19 stderr_truncated=false`
		if got != want {
			t.Fatalf("diagnostics=%q want %q", got, want)
		}
	})
	t.Run("unavailable stderr", func(t *testing.T) {
		got := windowsArchitectureProbeFailureOutput(nil, context.DeadlineExceeded)
		want := `stdout_prefix="" stdout_bytes=0 stdout_truncated=false stderr_prefix="" stderr_available_bytes=0 stderr_truncated=false`
		if got != want {
			t.Fatalf("diagnostics=%q want %q", got, want)
		}
	})
	t.Run("bounded prefixes", func(t *testing.T) {
		output := []byte(strings.Repeat("o", architectureProbeLimit) + "stdout-tail")
		stderr := []byte(strings.Repeat("e", architectureProbeLimit) + "stderr-tail")
		got := windowsArchitectureProbeFailureOutput(output, &exec.ExitError{Stderr: stderr})
		if strings.Contains(got, "stdout-tail") || strings.Contains(got, "stderr-tail") {
			t.Fatal("diagnostic output exceeded its prefix limit")
		}
		for _, field := range []string{
			fmt.Sprintf("stdout_prefix=%q", output[:architectureProbeLimit]),
			fmt.Sprintf("stdout_bytes=%d stdout_truncated=true", len(output)),
			fmt.Sprintf("stderr_prefix=%q", stderr[:architectureProbeLimit]),
			fmt.Sprintf("stderr_available_bytes=%d stderr_truncated=true", len(stderr)),
		} {
			if !strings.Contains(got, field) {
				t.Fatal("missing bounded prefix or truthful truncation metadata")
			}
		}
	})
}
