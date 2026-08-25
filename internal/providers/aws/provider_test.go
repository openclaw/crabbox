package aws

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderAdvertisesRunSessionContract(t *testing.T) {
	spec := (Provider{}).Spec()
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCleanup, core.FeatureRunSession} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
	if err := core.ValidateRunSessionFeatureSpec(spec); err != nil {
		t.Fatalf("run-session contract: %v", err)
	}
}

func TestAWSLinuxReadinessWaitsForCurrentBootCloudInit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executing Linux readiness checks requires a POSIX shell")
	}

	target := core.SSHTargetFromConfig(core.Config{Provider: "aws", TargetOS: core.TargetLinux}, "example.test")
	dir := t.TempDir()
	ready := filepath.Join(dir, "crabbox-ready")
	invoked := filepath.Join(dir, "ready-invoked")
	cloudInit := filepath.Join(dir, "cloud-init")
	timeout := filepath.Join(dir, "timeout")
	if err := os.WriteFile(cloudInit, []byte("#!/bin/sh\ntest \"$1\" = status && test \"$2\" = --wait || exit 2\nprintf w >&4\nIFS= read -r current_boot <&3\ntest \"$current_boot\" = complete\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(timeout, []byte("#!/bin/sh\ntest \"$1\" = 20m || exit 2\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf ready > %q\n", invoked)
	if err := os.WriteFile(ready, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	command := strings.NewReplacer(
		"/usr/local/bin/crabbox-ready", ready,
		"/tmp/crabbox-ready.log", filepath.Join(dir, "ready.log"),
		"/tmp/crabbox-cloud-init.log", filepath.Join(dir, "cloud-init.log"),
	).Replace(target.ReadyCheck)
	completionReader, completionWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = completionWriter.Close() })
	startedReader, startedWriter, err := os.Pipe()
	if err != nil {
		_ = completionReader.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = startedReader.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.ExtraFiles = []*os.File{completionReader, startedWriter}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		_ = completionReader.Close()
		_ = startedWriter.Close()
		t.Fatal(err)
	}
	_ = completionReader.Close()
	_ = startedWriter.Close()
	if _, err := io.ReadFull(startedReader, make([]byte, 1)); err != nil {
		commandErr := cmd.Wait()
		t.Fatalf("ready check did not wait for current-boot cloud-init: signal=%v command=%v output=%s", err, commandErr, output.String())
	}
	if _, err := os.Stat(invoked); !os.IsNotExist(err) {
		cancel()
		_ = cmd.Wait()
		t.Fatalf("crabbox-ready ran before cloud-init completed the current boot: %v", err)
	}
	if _, err := io.WriteString(completionWriter, "complete\n"); err != nil {
		cancel()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	_ = completionWriter.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("ready check rejected completed current boot: %v: %s", err, output.String())
	}
	if _, err := os.Stat(invoked); err != nil {
		t.Fatalf("ready check did not execute crabbox-ready after cloud-init completed: %v", err)
	}
}

func TestAWSCloudInitReadinessDoesNotChangeOtherTargets(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider string
		targetOS string
	}{
		{name: "AWS Windows", provider: "aws", targetOS: core.TargetWindows},
		{name: "AWS macOS", provider: "aws", targetOS: core.TargetMacOS},
		{name: "other Linux provider", provider: "hetzner", targetOS: core.TargetLinux},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := core.SSHTargetFromConfig(core.Config{Provider: test.provider, TargetOS: test.targetOS}, "example.test")
			if target.ReadyCheck != "" {
				t.Fatalf("ready check=%q, want unchanged default", target.ReadyCheck)
			}
		})
	}
}

func TestNativeCheckpointCapabilitySupportsWindowsImages(t *testing.T) {
	req := core.NativeCheckpointRequest{
		Server:           core.Server{CloudID: "i-123"},
		Target:           core.SSHTarget{TargetOS: core.TargetWindows, WindowsMode: core.WindowsModeNormal},
		Strategy:         core.CheckpointStrategyImage,
		StrategyExplicit: true,
	}

	direct, ok := (Provider{}).NativeCheckpointCapability(req)
	if !ok || direct.Kind != core.CheckpointKindAWSAMI || !direct.Direct {
		t.Fatalf("direct capability=%#v ok=%v, want direct AWS AMI", direct, ok)
	}

	req.Config.Coordinator = "https://coordinator.example"
	brokered, ok := (Provider{}).NativeCheckpointCapability(req)
	if !ok || brokered.Kind != core.CheckpointKindAWSAMI || brokered.Direct {
		t.Fatalf("brokered capability=%#v ok=%v, want brokered AWS AMI", brokered, ok)
	}

	req.Strategy = core.CheckpointStrategyDiskSnapshot
	if capability, ok := (Provider{}).NativeCheckpointCapability(req); ok {
		t.Fatalf("disk snapshot capability=%#v, want unsupported", capability)
	}

	req.StrategyExplicit = false
	automatic, ok := (Provider{}).NativeCheckpointCapability(req)
	if !ok || automatic.Kind != core.CheckpointKindAWSAMI || automatic.Direct {
		t.Fatalf("automatic capability=%#v ok=%v, want brokered AWS AMI", automatic, ok)
	}
}

func TestPrepareLeaseClaimEndpointPreservesCleanupIdentity(t *testing.T) {
	existing := core.LeaseClaim{
		LeaseID: "cbx_123456789abc",
		CloudID: "i-123",
		Slug:    "example",
		Labels: map[string]string{
			"lease":           "cbx_123456789abc",
			"slug":            "example",
			"aws_key_pair_id": "key-original",
			"aws_account_id":  "123456789012",
		},
	}
	server := core.Server{
		Provider: "aws",
		CloudID:  "i-123",
		Labels: map[string]string{
			"provider": "aws",
			"lease":    "cbx_123456789abc",
			"slug":     "example",
			"state":    "running",
		},
	}

	prepared, err := (Provider{}).PrepareLeaseClaimEndpoint(existing, "aws", "example", server, false)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Labels["aws_key_pair_id"] != "key-original" || prepared.Labels["aws_account_id"] != "123456789012" {
		t.Fatalf("labels=%v, want preserved cleanup identity", prepared.Labels)
	}
	server.Labels["aws_key_pair_id"] = "key-replacement"
	if _, err := (Provider{}).PrepareLeaseClaimEndpoint(existing, "aws", "example", server, false); err == nil {
		t.Fatal("expected mismatched immutable key identity rejection")
	}

	legacy := existing
	legacy.Labels = map[string]string{"lease": existing.LeaseID, "slug": existing.Slug}
	server.Labels["aws_key_pair_id"] = "key-cloud-tag"
	server.Labels["aws_account_id"] = "999999999999"
	prepared, err = (Provider{}).PrepareLeaseClaimEndpoint(legacy, "aws", "example", server, false)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Labels["aws_key_pair_id"] != "" || prepared.Labels["aws_account_id"] != "" {
		t.Fatalf("labels=%v, must not promote mutable cloud tags into cleanup authority", prepared.Labels)
	}
}
