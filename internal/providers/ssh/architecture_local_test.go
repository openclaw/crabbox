package ssh

import (
	"context"
	"os/exec"
	"runtime"
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
