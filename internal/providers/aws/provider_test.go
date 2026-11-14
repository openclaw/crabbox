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

func TestAWSSSHTransportValidation(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
		fail  bool
	}{
		{value: "", want: core.AWSSSHTransportDirect},
		{value: " DIRECT ", want: core.AWSSSHTransportDirect},
		{value: "SSM", want: core.AWSSSHTransportSSM},
		{value: "custom", fail: true},
	} {
		cfg := core.Config{AWSSSHTransport: test.value}
		err := (Provider{}).ApplyFlags(&cfg, nil, nil)
		if test.fail {
			if err == nil {
				t.Fatalf("transport %q accepted", test.value)
			}
			continue
		}
		if err != nil || cfg.AWSSSHTransport != test.want {
			t.Fatalf("transport %q normalized to %q, err=%v", test.value, cfg.AWSSSHTransport, err)
		}
	}
}

func TestAWSSSMTargetUsesDocumentedCLIArgv(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	awsPath := filepath.Join(dir, "aws")
	if err := os.WriteFile(awsPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_PATH\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ARGS_PATH", argsPath)
	region := "region with ' quote"
	if output, err := exec.Command("sh", "-c", awsSSMProxyCommand(region)).CombinedOutput(); err != nil {
		t.Fatalf("proxy command: %v: %s", err, output)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"ssm", "start-session",
		"--target", "%h",
		"--document-name", "AWS-StartSSHSession",
		"--parameters", "portNumber=%p",
		"--region", region,
	}, "\n") + "\n"
	if string(data) != want {
		t.Fatalf("aws argv=%q want=%q", data, want)
	}

	server := core.Server{CloudID: "i-0123456789abcdef0"}
	server.PublicNet.IPv4.IP = "203.0.113.10"
	target := awsSSHTarget(core.Config{Provider: "aws", AWSSSHTransport: core.AWSSSHTransportSSM, AWSRegion: "eu-west-1"}, server, server.PublicNet.IPv4.IP)
	if target.Host != server.CloudID || !target.NoControlMaster || !strings.Contains(target.ProxyCommand, "AWS-StartSSHSession") || strings.Contains(target.ProxyCommand, server.PublicNet.IPv4.IP) {
		t.Fatalf("SSM target=%+v", target)
	}
	direct := awsSSHTarget(core.Config{Provider: "aws", AWSSSHTransport: core.AWSSSHTransportDirect}, server, server.PublicNet.IPv4.IP)
	if direct.Host != server.PublicNet.IPv4.IP || direct.ProxyCommand != "" {
		t.Fatalf("direct target=%+v", direct)
	}
}

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

func TestAWSReadinessStatusUsesExistingBootOwners(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX guest observer")
	}
	for _, tc := range []struct {
		name, failed, pid, expected, activating string
		workspace, bootstrapped                 bool
	}{
		{name: "slow ownership repair", pid: "123", expected: "pending ownership-repair"},
		{name: "bootstrap pending", pid: "0", expected: "pending bootstrap"},
		{name: "cloud config pending", activating: "cloud-config.service", expected: "pending cloud-config"},
		{name: "cloud final pending", activating: "cloud-final.service", bootstrapped: true, expected: "pending cloud-final"},
		{name: "bootstrap terminal", failed: "crabbox-bootstrap.service", expected: "failed bootstrap"},
		{name: "cloud final terminal", failed: "cloud-final.service", expected: "failed cloud-final"},
		{name: "workspace terminal", failed: "crabbox-workspace-ready.service", expected: "failed workspace-ready"},
		{name: "stale bootstrap marker still needs current boot", bootstrapped: true, expected: "pending workspace-ready"},
		{name: "marker never establishes readiness", workspace: true, expected: "pending readiness-check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, script := range map[string]string{
				"systemctl": "#!/bin/sh\ncase \"$1\" in is-failed) [ \"$3\" = \"$FIXTURE_FAILED\" ];; show) if [ \"$2\" = --property=ActiveState ]; then [ \"$4\" = \"$FIXTURE_ACTIVATING\" ] && echo activating; else printf '%s\\n' \"$FIXTURE_PID\"; fi;; cat) exit 0;; *) exit 1;; esac\n",
				"pgrep":     "#!/bin/sh\n[ \"$1 $2 $3 $4\" = '-P 123 -x chown' ]\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
			}
			for path, present := range map[string]bool{"workspace": tc.workspace, "bootstrapped": tc.bootstrapped} {
				if present {
					if err := os.WriteFile(filepath.Join(dir, path), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			script := strings.NewReplacer("/run/crabbox/workspace-ready", filepath.Join(dir, "workspace"), "/var/lib/crabbox/bootstrapped", filepath.Join(dir, "bootstrapped")).Replace(awsLinuxReadinessStatus)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "FIXTURE_FAILED="+tc.failed, "FIXTURE_PID="+tc.pid, "FIXTURE_ACTIVATING="+tc.activating)
			out, err := cmd.CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != tc.expected {
				t.Fatalf("observer=%q error=%v want %q", out, err, tc.expected)
			}
		})
	}
}
