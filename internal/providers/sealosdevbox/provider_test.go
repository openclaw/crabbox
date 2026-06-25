package sealosdevbox

import (
	"context"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func testConfig() core.Config {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.SealosDevbox.Context = "sealos-context"
	cfg.SealosDevbox.Namespace = "team-a"
	cfg.SealosDevbox.Image = "ubuntu:24.04"
	cfg.SealosDevbox.TemplateID = "tpl-devbox"
	cfg.SealosDevbox.SSHGatewayHost = "ssh.sealos.example.test"
	return cfg
}

func TestProviderSpecAndAutomationSurface(t *testing.T) {
	provider := Provider{}
	if provider.Name() != providerName {
		t.Fatalf("name=%q", provider.Name())
	}
	if len(provider.Aliases()) != 0 {
		t.Fatalf("aliases=%v", provider.Aliases())
	}
	spec := provider.Spec()
	if spec.Name != providerName || spec.Family != familyName {
		t.Fatalf("spec=%#v", spec)
	}
	if spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec kind/coordinator=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
	if AutomationSurfaceDecision != "crd_first" {
		t.Fatalf("automation surface=%q", AutomationSurfaceDecision)
	}
}

func TestFlagsExpandLocalPathsAndPreserveGuestWorkRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := testConfig()
	fs := flag.NewFlagSet("sealos-devbox", flag.ContinueOnError)
	values := registerFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--sealos-devbox-kubectl=~/bin/kubectl",
		"--sealos-devbox-kubeconfig=~/.kube/sealos.yaml",
		"--sealos-devbox-context=sealos-flag",
		"--sealos-devbox-namespace=team-flag",
		"--sealos-devbox-network=NodePort",
		"--sealos-devbox-node-host=node.example.test",
		"--sealos-devbox-work-root=/home/devbox/~/guest-project",
		"--sealos-devbox-delete-on-release",
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.SealosDevbox.Kubectl != filepath.Join(home, "bin/kubectl") {
		t.Fatalf("kubectl=%q", cfg.SealosDevbox.Kubectl)
	}
	if cfg.SealosDevbox.Kubeconfig != filepath.Join(home, ".kube/sealos.yaml") {
		t.Fatalf("kubeconfig=%q", cfg.SealosDevbox.Kubeconfig)
	}
	if cfg.SealosDevbox.WorkRoot != "/home/devbox/~/guest-project" {
		t.Fatalf("guest workRoot was shell-expanded: %q", cfg.SealosDevbox.WorkRoot)
	}
	if !core.DeleteOnReleaseExplicit(cfg, providerName) {
		t.Fatal("deleteOnRelease flag was not marked explicit")
	}
}

func TestValidateRejectsDeferredTailnet(t *testing.T) {
	cfg := testConfig()
	cfg.SealosDevbox.Network = "Tailnet"
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "SSHGate or NodePort") {
		t.Fatalf("validate error=%v", err)
	}
}

func TestConfigureRejectsNonLinuxTarget(t *testing.T) {
	cfg := testConfig()
	cfg.TargetOS = core.TargetMacOS
	if _, err := (Provider{}).Configure(cfg, core.Runtime{Exec: &recordingRunner{}}); err == nil {
		t.Fatal("non-linux target accepted")
	}
}

func TestConfigureRejectsMissingRouteConfig(t *testing.T) {
	cfg := testConfig()
	cfg.SealosDevbox.SSHGatewayHost = ""
	if err := (Provider{}).ValidateConfig(cfg); err != nil {
		t.Fatalf("base validation should allow doctor preflight: %v", err)
	}
	if _, err := (Provider{}).Configure(cfg, core.Runtime{Exec: &recordingRunner{}}); err == nil || !strings.Contains(err.Error(), "sshGatewayHost") {
		t.Fatalf("configure error=%v", err)
	}
}

func TestDoctorReportsMissingSSHGateRoute(t *testing.T) {
	cfg := lifecycleConfig()
	cfg.SealosDevbox.SSHGatewayHost = ""
	doctor, err := (Provider{}).ConfigureDoctor(cfg, core.Runtime{Exec: &recordingRunner{}, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	check := findDoctorCheck(t, result.Checks, "network")
	if result.Status != "blocked" || check.Status != "failed" || check.Details["host_configured"] != "false" {
		t.Fatalf("result=%#v check=%#v", result, check)
	}
}

func TestDoctorReportsMissingNodePortRoute(t *testing.T) {
	cfg := lifecycleConfig()
	cfg.SealosDevbox.Network = networkNodePort
	cfg.SealosDevbox.NodeHost = ""
	doctor, err := (Provider{}).ConfigureDoctor(cfg, core.Runtime{Exec: &recordingRunner{}, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	check := findDoctorCheck(t, result.Checks, "network")
	if result.Status != "blocked" || check.Status != "failed" || check.Details["node_host_configured"] != "false" {
		t.Fatalf("result=%#v check=%#v", result, check)
	}
}

func TestDoctorUsesReadOnlyKubectlCommands(t *testing.T) {
	runner := &recordingRunner{}
	doctor, err := (Provider{}).ConfigureDoctor(lifecycleConfig(), core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != providerName || result.Status != "ready" {
		t.Fatalf("result=%#v", result)
	}
	if !strings.Contains(result.Message, "automation_surface=crd_first") || !strings.Contains(result.Message, "mutation=false") {
		t.Fatalf("message=%q", result.Message)
	}
	if len(runner.requests) == 0 {
		t.Fatal("doctor did not run kubectl")
	}
	for _, req := range runner.requests {
		args := req.Args
		if len(args) == 0 {
			t.Fatalf("empty kubectl args: %#v", req)
		}
		commandStart := firstKubectlVerb(args)
		if commandStart < 0 {
			t.Fatalf("could not find kubectl verb in %v", args)
		}
		verb := args[commandStart]
		if verb == "auth" && commandStart+1 < len(args) && args[commandStart+1] == "can-i" {
			continue
		}
		switch verb {
		case "apply", "create", "delete", "patch", "replace", "scale", "rollout", "edit":
			t.Fatalf("doctor used mutating command: %s", commandString(req))
		}
	}
}

func TestDoctorRedactsSensitiveCommandOutput(t *testing.T) {
	runner := &recordingRunner{
		errors: map[int]error{3: errors.New("exit status 1")},
		results: map[int]core.LocalCommandResult{
			3: {ExitCode: 1, Stderr: "token=secret-token private_key=secret-key"},
		},
	}
	doctor, err := (Provider{}).ConfigureDoctor(testConfig(), core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "blocked" {
		t.Fatalf("result=%#v", result)
	}
	text := ""
	for _, check := range result.Checks {
		text += check.Message + "\n"
	}
	if strings.Contains(text, "secret-token") || strings.Contains(text, "secret-key") {
		t.Fatalf("doctor leaked sensitive text: %s", text)
	}
	if !strings.Contains(text, "[redacted]") {
		t.Fatalf("redaction marker missing: %s", text)
	}
}

type recordingRunner struct {
	requests []core.LocalCommandRequest
	results  map[int]core.LocalCommandResult
	errors   map[int]error
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.requests = append(r.requests, req)
	index := len(r.requests)
	if result, ok := r.results[index]; ok {
		return result, r.errors[index]
	}
	return core.LocalCommandResult{Stdout: "ok"}, r.errors[index]
}

func firstKubectlVerb(args []string) int {
	for i, arg := range args {
		if strings.HasPrefix(arg, "-") {
			if arg == "--kubeconfig" || arg == "--context" || arg == "--namespace" {
				i++
			}
			continue
		}
		if i > 0 {
			prev := args[i-1]
			if prev == "--kubeconfig" || prev == "--context" || prev == "--namespace" {
				continue
			}
		}
		return i
	}
	return -1
}

func findDoctorCheck(t *testing.T, checks []core.DoctorCheck, name string) core.DoctorCheck {
	t.Helper()
	for _, check := range checks {
		if check.Check == name {
			return check
		}
	}
	t.Fatalf("doctor check %q missing from %#v", name, checks)
	return core.DoctorCheck{}
}
