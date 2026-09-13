package nebius

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	calls [][]string
	fn    func(core.LocalCommandRequest) (core.LocalCommandResult, error)
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	call := append([]string{req.Name}, req.Args...)
	r.calls = append(r.calls, call)
	if r.fn != nil {
		return r.fn(req)
	}
	return core.LocalCommandResult{}, nil
}

func testConfig() core.Config {
	return core.Config{
		Provider: providerName,
		TargetOS: targetLinux,
		SSHUser:  "crabbox",
		SSHPort:  "22",
		WorkRoot: "/tmp/crabbox",
		Nebius: core.NebiusConfig{
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

func TestNebiusOrdinaryFlagMetadata(t *testing.T) {
	initial := core.NebiusConfig{CLI: "nebius", Profile: "profile", ParentID: "parent", SubnetID: "subnet", Platform: "cpu-d3", Preset: "4vcpu-16gb", ImageFamily: "ubuntu24.04-driverless", DiskType: "network_ssd", DiskSizeGiB: 50, User: "crabbox", PublicIP: "dynamic", SecurityGroupIDs: []string{"prior"}, ServiceAccountID: "account", RecoveryPolicy: "fail"}
	for _, tc := range []struct {
		raw    string
		groups []string
		disk   int
	}{{"", nil, 0}, {", ,", nil, -2}, {"none", []string{"none"}, 50}, {" a, ,b,a ", []string{"a", "b", "a"}, 50}} {
		t.Run(fmt.Sprintf("%q", tc.raw), func(t *testing.T) {
			cfg := core.Config{Provider: "other", SSHUser: "generic", WorkRoot: "/workspace/generic", ServerType: "generic", Nebius: initial}
			before := cfg
			fs := flag.NewFlagSet("metadata", flag.ContinueOnError)
			values := (Provider{}).RegisterFlags(fs, cfg)
			if fs.Lookup("nebius-security-group-ids").DefValue != "" {
				t.Fatal("group scalar inherited nonempty default")
			}
			if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg, before) {
				t.Fatal("unvisited fields changed")
			}
			if err := fs.Parse([]string{"--nebius-cli=~/literal", "--nebius-profile= profile ", "--nebius-parent-id=parent", "--nebius-subnet-id=subnet", "--nebius-platform=cpu-d3", "--nebius-preset=4vcpu-16gb", "--nebius-image-family=ubuntu24.04-driverless", "--nebius-disk-type=network_ssd", fmt.Sprintf("--nebius-disk-size-gib=%d", tc.disk), "--nebius-user=crabbox", "--nebius-public-ip=dynamic", "--nebius-security-group-ids=first", "--nebius-security-group-ids=" + tc.raw, "--nebius-service-account-id=account", "--nebius-recovery-policy=fail"}); err != nil {
				t.Fatal(err)
			}
			if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			want := before
			want.Nebius.CLI = "~/literal"
			want.Nebius.Profile = " profile "
			want.Nebius.DiskSizeGiB = tc.disk
			want.Nebius.SecurityGroupIDs = tc.groups
			core.RecordProviderFlagInputs(&want, true, "nebius")
			if !reflect.DeepEqual(cfg, want) {
				t.Fatal("all-field raw/equal/repeated/list/ledger application changed generic state")
			}
		})
	}
	for _, name := range []string{"nebius", "Nebius", " nebius ", "other"} {
		cfg := core.Config{Provider: name, Nebius: initial}
		fs := flag.NewFlagSet("validation", flag.ContinueOnError)
		values := (Provider{}).RegisterFlags(fs, cfg)
		if err := fs.Parse([]string{"--nebius-disk-size-gib=-1"}); err != nil {
			t.Fatal(err)
		}
		before := cfg
		for _, foreign := range []any{nil, struct{}{}} {
			if err := (Provider{}).ApplyFlags(&cfg, fs, foreign); err != nil || !reflect.DeepEqual(cfg, before) {
				t.Fatal("foreign values reached validation or mutation")
			}
		}
		err := (Provider{}).ApplyFlags(&cfg, fs, values)
		if name == "nebius" {
			if err == nil || err.Error() != "nebius.diskSizeGiB must be positive" {
				t.Fatalf("selected validation=%v", err)
			}
		} else if err != nil {
			t.Fatalf("other spelling unexpectedly validated: %v", err)
		}
		want := before
		want.Nebius.DiskSizeGiB = -1
		core.RecordProviderFlagInputs(&want, true, "nebius")
		if !reflect.DeepEqual(cfg, want) {
			t.Fatal("assignment before selected validation changed")
		}
	}
	cfg := core.Config{Provider: "other", Nebius: initial}
	fs := flag.NewFlagSet("empty", flag.ContinueOnError)
	values := (Provider{}).RegisterFlags(fs, cfg)
	var args []string
	for _, name := range []string{"cli", "profile", "parent-id", "subnet-id", "platform", "preset", "image-family", "disk-type", "user", "public-ip", "security-group-ids", "service-account-id", "recovery-policy"} {
		args = append(args, "--nebius-"+name+"=")
	}
	args = append(args, "--nebius-disk-size-gib=0")
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	want := core.Config{Provider: "other"}
	core.RecordProviderFlagInputs(&want, true, "nebius")
	if !reflect.DeepEqual(cfg, want) {
		t.Fatal("empty visited fields did not clear without extra validation")
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
	for _, user := range []string{"root", "admin", "alice\n  - name: root", "Alice", "bad user", "1alice"} {
		cfg := testConfig()
		cfg.Nebius.User = user
		if err := (Provider{}).ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "nebius.user must") {
			t.Fatalf("ValidateConfig(%q) err=%v", user, err)
		}
	}
}

func TestCLIRunnerAddsProfileBeforeCommand(t *testing.T) {
	runner := &recordingRunner{}
	cfg := testConfig()
	client := newCLIRunner(cfg.Nebius, core.Runtime{Exec: runner})
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
	ok, err = containsIDOrName(`{"metadata":{"id":"subnet-123","name":"default"}}`, "subnet-123")
	if err != nil || !ok {
		t.Fatalf("containsIDOrName metadata.id ok=%v err=%v", ok, err)
	}
	ok, err = containsIDOrName(`{"items":[{"metadata":{"name":"cpu-d3"}}]}`, "cpu-d3")
	if err != nil || !ok {
		t.Fatalf("containsIDOrName wrapped metadata.name ok=%v err=%v", ok, err)
	}
	ok, err = containsIDOrName(`{"items":[{"metadata":{"id":"image-opaque"},"spec":{"family":"ubuntu24.04-driverless"}}]}`, "ubuntu24.04-driverless")
	if err != nil || !ok {
		t.Fatalf("containsIDOrName later family ok=%v err=%v", ok, err)
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
	runner := &recordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		joined := strings.Join(req.Args, " ")
		for _, forbidden := range []string{" create", " delete", " update", " disk create", " disk delete", " allocation create", " allocation delete"} {
			if strings.Contains(" "+joined, forbidden) {
				return core.LocalCommandResult{}, errors.New("mutating command invoked: " + joined)
			}
		}
		switch joined {
		case "--profile sandbox version":
			return core.LocalCommandResult{Stdout: "nebius version 1.0.0\n"}, nil
		case "--profile sandbox profile list":
			return core.LocalCommandResult{Stdout: "sandbox [default]\n"}, nil
		case "--profile sandbox iam project get project-123 --format json":
			return core.LocalCommandResult{Stdout: `{"id":"project-123"}`}, nil
		case "--profile sandbox vpc subnet list --parent-id project-123 --format json":
			return core.LocalCommandResult{Stdout: `[{"id":"subnet-123"}]`}, nil
		case "--profile sandbox compute platform list --parent-id project-123 --format json":
			return core.LocalCommandResult{Stdout: `[{"id":"cpu-d3"}]`}, nil
		case "--profile sandbox compute image get-latest-by-family --image-family ubuntu24.04-driverless --format json":
			return core.LocalCommandResult{Stdout: `{"metadata":{"id":"image-123"}}`}, nil
		case "--profile sandbox compute instance list --parent-id project-123 --format json":
			return core.LocalCommandResult{Stdout: `{"items":[]}`}, nil
		default:
			return core.LocalCommandResult{}, errors.New("unexpected command: " + joined)
		}
	}}
	backend := NewBackend(Provider{}.Spec(), testConfig(), core.Runtime{Exec: runner}).(*backend)
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
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

func TestDoctorRejectsMissingImageFamily(t *testing.T) {
	runner := &recordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		joined := strings.Join(req.Args, " ")
		switch joined {
		case "--profile sandbox version":
			return core.LocalCommandResult{Stdout: "nebius version 1.0.0\n"}, nil
		case "--profile sandbox profile list":
			return core.LocalCommandResult{Stdout: "sandbox [default]\n"}, nil
		case "--profile sandbox iam project get project-123 --format json":
			return core.LocalCommandResult{Stdout: `{"id":"project-123"}`}, nil
		case "--profile sandbox vpc subnet list --parent-id project-123 --format json":
			return core.LocalCommandResult{Stdout: `[{"metadata":{"id":"subnet-123"}}]`}, nil
		case "--profile sandbox compute platform list --parent-id project-123 --format json":
			return core.LocalCommandResult{Stdout: `[{"metadata":{"id":"cpu-d3"}}]`}, nil
		case "--profile sandbox compute image get-latest-by-family --image-family ubuntu24.04-driverless --format json":
			return core.LocalCommandResult{Stdout: `{}`}, nil
		case "--profile sandbox compute instance list --parent-id project-123 --format json":
			return core.LocalCommandResult{Stdout: `{"items":[]}`}, nil
		default:
			return core.LocalCommandResult{}, errors.New("unexpected command: " + joined)
		}
	}}
	backend := NewBackend(Provider{}.Spec(), testConfig(), core.Runtime{Exec: runner}).(*backend)
	result, err := backend.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "error" {
		t.Fatalf("status=%q checks=%#v", result.Status, result.Checks)
	}
	for _, check := range result.Checks {
		if check.Check == "image" {
			if check.Status != "error" || !strings.Contains(check.Message, "configured image family not found") {
				t.Fatalf("image check=%#v", check)
			}
			return
		}
	}
	t.Fatalf("missing image check: %#v", result.Checks)
}
