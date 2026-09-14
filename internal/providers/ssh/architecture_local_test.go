package ssh

import (
	"context"
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
	ctx, cancel := context.WithTimeout(context.Background(), architectureProbeTimeout)
	defer cancel()
	// Identify the same interpreter that evaluates the unchanged production script, with one cold start.
	command := `[Console]::WriteLine($PSVersionTable.PSEdition + '|' + $PSVersionTable.PSVersion.ToString())` + "\n" + windowsArchitectureProbe
	combinedOutput, err := exec.CommandContext(ctx, powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command).Output()
	if err != nil {
		t.Fatalf("local Windows architecture probe: %v (context: %v)", err, ctx.Err())
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
