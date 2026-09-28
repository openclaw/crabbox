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

func TestReadyPoolImageIdentityMatchesLease(t *testing.T) {
	request := core.ProviderReadyPoolImageIdentityRequest{
		Identity: core.CoordinatorReadyPoolImageIdentity{
			Provider: "aws", Scope: "us-east-1", ID: "ami-0123456789abcdef0",
		},
		Lease: core.ProviderReadyPoolLeaseImageIdentity{
			Provider: "aws", Region: "us-east-1",
			Image: &core.CoordinatorLeaseImage{
				Provider: "aws", Kind: "aws-ami", Region: "us-east-1", ID: "ami-0123456789abcdef0",
			},
		},
	}
	if !(Provider{}).ReadyPoolImageIdentityMatchesLease(request) {
		t.Fatal("valid AWS image identity rejected")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*core.ProviderReadyPoolImageIdentityRequest)
	}{
		{"identity provider", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Identity.Provider = "gcp" }},
		{"lease provider", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Lease.Provider = "gcp" }},
		{"missing image", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Lease.Image = nil }},
		{"image provider", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Lease.Image.Provider = "gcp" }},
		{"image kind", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Lease.Image.Kind = "aws-ebs" }},
		{"image id", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Lease.Image.ID = "ami-other" }},
		{"image region", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Lease.Image.Region = "eu-west-1" }},
		{"lease region", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Lease.Region = "eu-west-1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := request
			image := *request.Lease.Image
			changed.Lease.Image = &image
			tc.mutate(&changed)
			if (Provider{}).ReadyPoolImageIdentityMatchesLease(changed) {
				t.Fatalf("mismatched request accepted: %#v", changed)
			}
		})
	}
}

func TestAWSLinuxReadinessWaitsForCurrentBootCloudInit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executing Linux readiness checks requires a POSIX shell")
	}

	target := core.SSHTargetFromConfig(core.Config{Provider: "aws", TargetOS: core.TargetLinux}, "example.test")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

func TestAWSLinuxReadinessRequiresCurrentBootWorkspaceMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executing Linux readiness checks requires a POSIX shell")
	}
	for _, test := range []struct {
		name                                 string
		unit, workspace, bootstrap, optional bool
		cloudInitFails, wantReady            bool
	}{
		{name: "new guest skips cloud-init", unit: true, workspace: true, bootstrap: true, optional: true, cloudInitFails: true, wantReady: true},
		{name: "stale image bootstrap marker", unit: true, bootstrap: true, optional: true},
		{name: "bootstrap unfinished", unit: true, workspace: true, optional: true},
		{name: "optional cloud-final setup unfinished", unit: true, bootstrap: true},
		{name: "normal predicate still required", unit: true, workspace: true, bootstrap: true},
		{name: "optional setup finished", unit: true, workspace: true, bootstrap: true, optional: true, wantReady: true},
		{name: "legacy guest waits", bootstrap: true, optional: true, wantReady: true},
		{name: "legacy failed boot rejects stale markers", workspace: true, bootstrap: true, optional: true, cloudInitFails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, present := range map[string]bool{"unit": test.unit, "workspace-ready": test.workspace, "bootstrapped": test.bootstrap, "optional-ready": test.optional, "cloud-init-fails": test.cloudInitFails} {
				if present {
					if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			for name, script := range map[string]string{
				"systemctl":     "test \"$*\" = 'cat crabbox-workspace-ready.service' || exit 99\ntest -f unit\n",
				"cloud-init":    "touch cloud-init-invoked\ntest \"$*\" = 'status --wait' || exit 99\ntest ! -f cloud-init-fails\n",
				"timeout":       "test \"$1\" = 20m || exit 99\nshift\nexec \"$@\"\n",
				"crabbox-ready": "touch ready-invoked\ntest -f bootstrapped && test -f optional-ready\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			target := core.SSHTargetFromConfig(core.Config{Provider: "aws", TargetOS: core.TargetLinux}, "example.test")
			command := strings.NewReplacer(
				"/run/crabbox/workspace-ready", filepath.Join(dir, "workspace-ready"),
				"/usr/local/bin/crabbox-ready", filepath.Join(dir, "crabbox-ready"),
				"/tmp/crabbox-ready.log", filepath.Join(dir, "ready.log"),
				"/tmp/crabbox-cloud-init.log", filepath.Join(dir, "cloud-init.log"),
			).Replace(target.ReadyCheck)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", command)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil || (err == nil) != test.wantReady {
				t.Fatalf("readiness=%v want=%t: %s", err, test.wantReady, output)
			}
			for name, want := range map[string]bool{
				"cloud-init-invoked": !test.unit,
				"ready-invoked":      test.unit && test.workspace || !test.unit && !test.cloudInitFails,
			} {
				if _, err := os.Stat(filepath.Join(dir, name)); (err == nil) != want {
					t.Fatalf("%s=%v want invoked=%t", name, err, want)
				}
			}
		})
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
