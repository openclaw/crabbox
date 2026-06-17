package nebius

import (
	"context"
	"errors"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	calls [][]string
	fn    func(LocalCommandRequest) (LocalCommandResult, error)
}

func (r *recordingRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	call := append([]string{req.Name}, req.Args...)
	r.calls = append(r.calls, call)
	if r.fn != nil {
		return r.fn(req)
	}
	return LocalCommandResult{}, nil
}

func testConfig() Config {
	return Config{
		Provider: providerName,
		TargetOS: targetLinux,
		SSHUser:  "crabbox",
		SSHPort:  "22",
		WorkRoot: "/tmp/crabbox",
		Nebius: NebiusConfig{
			CLI:            "nebius",
			Profile:        "sandbox",
			ParentID:       "project-123",
			SubnetID:       "subnet-123",
			Platform:       "cpu-d3",
			Preset:         "4vcpu-16gb",
			ImageFamily:    "ubuntu24.04-driverless",
			DiskType:       "network_ssd",
			DiskSizeGiB:    50,
			User:           "crabbox",
			PublicIP:       "dynamic",
			RecoveryPolicy: "fail",
		},
	}
}

func TestProviderSpec(t *testing.T) {
	spec := Provider{}.Spec()
	if spec.Name != providerName || spec.Family != providerName || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
	if aliases := (Provider{}).Aliases(); len(aliases) != 0 {
		t.Fatalf("aliases=%v, want none", aliases)
	}
}

func TestApplyFlags(t *testing.T) {
	cfg := testConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := (Provider{}).RegisterFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--nebius-cli", "/opt/nebius",
		"--nebius-profile", "profile-a",
		"--nebius-parent-id", "project-a",
		"--nebius-subnet-id", "subnet-a",
		"--nebius-platform", "gpu-h100",
		"--nebius-preset", "1gpu-16vcpu-200gb",
		"--nebius-image-family", "ubuntu22.04-cuda",
		"--nebius-disk-type", "network_ssd_nonreplicated",
		"--nebius-disk-size-gib", "160",
		"--nebius-user", "alice",
		"--nebius-public-ip", "none",
		"--nebius-security-group-ids", "sg-a, sg-b",
		"--nebius-service-account-id", "sa-a",
		"--nebius-recovery-policy", "fail",
	}); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Nebius.CLI != "/opt/nebius" || cfg.Nebius.Profile != "profile-a" || cfg.Nebius.ParentID != "project-a" || cfg.Nebius.SubnetID != "subnet-a" {
		t.Fatalf("identity flags not applied: %#v", cfg.Nebius)
	}
	if cfg.Nebius.Platform != "gpu-h100" || cfg.Nebius.Preset != "1gpu-16vcpu-200gb" || cfg.Nebius.DiskSizeGiB != 160 || strings.Join(cfg.Nebius.SecurityGroupIDs, ",") != "sg-a,sg-b" {
		t.Fatalf("sizing/network flags not applied: %#v", cfg.Nebius)
	}
}

func TestValidateConfigRejectsReservedUsers(t *testing.T) {
	for _, user := range []string{"root", "admin"} {
		cfg := testConfig()
		cfg.Nebius.User = user
		if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "must not be root or admin") {
			t.Fatalf("ValidateConfig(%q) err=%v", user, err)
		}
	}
}

func TestCLIRunnerAddsProfileBeforeCommand(t *testing.T) {
	runner := &recordingRunner{}
	cfg := testConfig()
	client := newCLIRunner(cfg.Nebius, Runtime{Exec: runner})
	if _, err := client.run(context.Background(), "compute", "platform", "list", "--format", "json"); err != nil {
		t.Fatal(err)
	}
	want := []string{"nebius", "--profile", "sandbox", "compute", "platform", "list", "--format", "json"}
	if !reflect.DeepEqual(runner.calls[0], want) {
		t.Fatalf("call=%#v want %#v", runner.calls[0], want)
	}
}

func TestParseHelpers(t *testing.T) {
	ok, err := containsIDOrName(`[{"id":"subnet-123"},{"name":"cpu-d3"}]`, "cpu-d3")
	if err != nil || !ok {
		t.Fatalf("containsIDOrName name ok=%v err=%v", ok, err)
	}
	ok, err = containsIDOrName(`{"id":"project-123"}`, "project-123")
	if err != nil || !ok {
		t.Fatalf("containsIDOrName object ok=%v err=%v", ok, err)
	}
	if _, err := containsIDOrName(`not-json`, "x"); err == nil {
		t.Fatal("malformed JSON accepted")
	}
}

func TestRedactNebiusText(t *testing.T) {
	input := "request failed token=osb_secret private_key=-----BEGIN PRIVATE KEY-----"
	got := redactNebiusText(input)
	if strings.Contains(got, "osb_secret") || strings.Contains(got, "BEGIN PRIVATE KEY") {
		t.Fatalf("secret was not redacted: %q", got)
	}
}

func TestDoctorUsesReadOnlyCLICommands(t *testing.T) {
	runner := &recordingRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		joined := strings.Join(req.Args, " ")
		for _, forbidden := range []string{" create", " delete", " update", " compute instance", " disk create", " disk delete", " allocation create", " allocation delete"} {
			if strings.Contains(" "+joined, forbidden) {
				return LocalCommandResult{}, errors.New("mutating command invoked: " + joined)
			}
		}
		switch joined {
		case "--profile sandbox --version":
			return LocalCommandResult{Stdout: "nebius version 1.0.0\n"}, nil
		case "--profile sandbox config profile list --format json":
			return LocalCommandResult{Stdout: `[{"name":"sandbox","active":true}]`}, nil
		case "--profile sandbox iam project get project-123 --format json":
			return LocalCommandResult{Stdout: `{"id":"project-123"}`}, nil
		case "--profile sandbox vpc subnet list --parent-id project-123 --format json":
			return LocalCommandResult{Stdout: `[{"id":"subnet-123"}]`}, nil
		case "--profile sandbox compute platform list --format json":
			return LocalCommandResult{Stdout: `[{"id":"cpu-d3"}]`}, nil
		case "--profile sandbox compute image list --family ubuntu24.04-driverless --format json":
			return LocalCommandResult{Stdout: `[{"family":"ubuntu24.04-driverless"}]`}, nil
		case "--profile sandbox config get parent-id --format json":
			return LocalCommandResult{Stdout: `{"parent-id":"project-123"}`}, nil
		default:
			return LocalCommandResult{}, errors.New("unexpected command: " + joined)
		}
	}}
	backend := NewBackend(Provider{}.Spec(), testConfig(), Runtime{Exec: runner}).(*backend)
	result, err := backend.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || result.Status != "ok" {
		t.Fatalf("result=%#v", result)
	}
	if len(result.Checks) != 7 {
		t.Fatalf("checks=%#v", result.Checks)
	}
	if len(runner.calls) != 7 {
		t.Fatalf("calls=%#v", runner.calls)
	}
}
