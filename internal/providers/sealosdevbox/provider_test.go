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
	aliases := provider.Aliases()
	if len(aliases) != 2 || aliases[0] != "sealos" || aliases[1] != "sealos-dev" {
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

func TestCommandRoutingArgsPreservesEffectiveConfig(t *testing.T) {
	cfg := lifecycleConfig()
	cfg.SealosDevbox.Kubectl = "/opt/bin/kubectl"
	cfg.SealosDevbox.Kubeconfig = "/tmp/kube config"
	cfg.SealosDevbox.Context = "dev"
	cfg.SealosDevbox.Namespace = "team-devboxes"
	cfg.SealosDevbox.Network = networkNodePort
	cfg.SealosDevbox.NodeHost = "node.example.test"
	cfg.SealosDevbox.SSHGatewayHost = "gateway.example.test"
	cfg.SealosDevbox.SSHGatewayPort = "2222"
	cfg.SealosDevbox.SSHUser = "alice"
	cfg.SealosDevbox.WorkRoot = "/home/alice/project"
	cfg.SealosDevbox.DeleteOnRelease = false
	core.MarkDeleteOnReleaseExplicit(&cfg, providerName)

	got := strings.Join((Provider{}).CommandRoutingArgs(cfg, "cbx_abcdef123456"), "\n")
	for _, want := range []string{
		"--sealos-devbox-kubectl\n/opt/bin/kubectl",
		"--sealos-devbox-kubeconfig\n/tmp/kube config",
		"--sealos-devbox-context\ndev",
		"--sealos-devbox-namespace\nteam-devboxes",
		"--sealos-devbox-network\nNodePort",
		"--sealos-devbox-node-host\nnode.example.test",
		"--sealos-devbox-ssh-gateway-host\ngateway.example.test",
		"--sealos-devbox-ssh-gateway-port\n2222",
		"--sealos-devbox-ssh-user\nalice",
		"--sealos-devbox-work-root\n/home/alice/project",
		"--sealos-devbox-delete-on-release=false",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("routing args missing %q:\n%s", want, got)
		}
	}
}

func TestCommandRoutingArgsUsesExplicitTopLevelWorkRoot(t *testing.T) {
	cfg := lifecycleConfig()
	cfg.WorkRoot = "/srv/crabbox"
	core.MarkWorkRootExplicit(&cfg)

	got := strings.Join((Provider{}).CommandRoutingArgs(cfg, "cbx_abcdef123456"), "\n")
	if !strings.Contains(got, "--sealos-devbox-work-root\n/srv/crabbox") {
		t.Fatalf("routing args lost explicit work root:\n%s", got)
	}
	if strings.Contains(got, "--sealos-devbox-work-root\n/home/devbox/project") {
		t.Fatalf("routing args retained stale provider work root:\n%s", got)
	}
}

func TestCommandRoutingArgsPrefersExplicitProviderWorkRoot(t *testing.T) {
	cfg := lifecycleConfig()
	cfg.WorkRoot = "/srv/crabbox"
	core.MarkWorkRootExplicit(&cfg)
	cfg.SealosDevbox.WorkRoot = "/home/devbox/override"
	core.MarkSealosDevboxWorkRootExplicit(&cfg)

	got := strings.Join((Provider{}).CommandRoutingArgs(cfg, "cbx_abcdef123456"), "\n")
	if !strings.Contains(got, "--sealos-devbox-work-root\n/home/devbox/override") {
		t.Fatalf("routing args lost provider work root:\n%s", got)
	}
	if strings.Contains(got, "--sealos-devbox-work-root\n/srv/crabbox") {
		t.Fatalf("routing args used lower-priority generic work root:\n%s", got)
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
	if !core.IsSealosDevboxWorkRootExplicit(&cfg) {
		t.Fatal("workRoot flag was not marked explicit")
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

func TestConfigurePreservesExplicitTopLevelWorkRoot(t *testing.T) {
	cfg := testConfig()
	cfg.WorkRoot = "/srv/crabbox"
	core.MarkWorkRootExplicit(&cfg)
	configured, err := (Provider{}).Configure(cfg, core.Runtime{Exec: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := configured.(*backend).cfg.WorkRoot; got != "/srv/crabbox" {
		t.Fatalf("work root=%q want explicit root", got)
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
	crdCheck := findDoctorCheck(t, result.Checks, "crd.devboxes")
	if crdCheck.Status != "ok" || crdCheck.Details["groupVersion"] != devboxGroupVersion || crdCheck.Details["resource"] != "devboxes" {
		t.Fatalf("crd check=%#v", crdCheck)
	}
	if got := commandString(runner.requests[3]); !strings.Contains(got, "get --raw /apis/"+devboxGroupVersion) {
		t.Fatalf("CRD check did not use tenant-safe API discovery: %s", got)
	}
	if !strings.Contains(result.Message, "automation_surface=crd_first") || !strings.Contains(result.Message, "mutation=false") {
		t.Fatalf("message=%q", result.Message)
	}
	patchCheck := findDoctorCheck(t, result.Checks, "rbac.patch.devboxes")
	if patchCheck.Status != "ok" {
		t.Fatalf("patch RBAC check=%#v", patchCheck)
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
		if isRulesReviewRequest(req) {
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

func TestDoctorRequiresExplicitRBACYes(t *testing.T) {
	runner := &recordingRunner{
		results: map[int]core.LocalCommandResult{
			5: {Stdout: `{"status":{"resourceRules":[]}}`},
		},
	}
	doctor, err := (Provider{}).ConfigureDoctor(lifecycleConfig(), core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	check := findDoctorCheck(t, result.Checks, "rbac.get.devboxes")
	if result.Status != "blocked" || check.Status != "failed" || check.Message != "denied" || check.Details["allowed"] != "false" {
		t.Fatalf("result=%#v check=%#v", result, check)
	}
}

func TestRulesAllowRequiresUnscopedMatchingRule(t *testing.T) {
	tests := []struct {
		name     string
		rules    []resourceRule
		verb     string
		group    string
		resource string
		want     bool
	}{
		{
			name:     "exact core rule",
			rules:    []resourceRule{{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"pods"}}},
			verb:     "get",
			resource: "pods",
			want:     true,
		},
		{
			name:     "wildcards",
			rules:    []resourceRule{{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}}},
			verb:     "delete",
			group:    "devbox.sealos.io",
			resource: "devboxes",
			want:     true,
		},
		{
			name:     "wrong group",
			rules:    []resourceRule{{Verbs: []string{"get"}, APIGroups: []string{"apps"}, Resources: []string{"devboxes"}}},
			verb:     "get",
			group:    "devbox.sealos.io",
			resource: "devboxes",
		},
		{
			name:     "resource name restricted",
			rules:    []resourceRule{{Verbs: []string{"get"}, APIGroups: []string{"devbox.sealos.io"}, Resources: []string{"devboxes"}, ResourceNames: []string{"one"}}},
			verb:     "get",
			group:    "devbox.sealos.io",
			resource: "devboxes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rulesAllow(tt.rules, tt.verb, tt.group, tt.resource); got != tt.want {
				t.Fatalf("rulesAllow()=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestDoctorRequiresDevboxCRDVersion(t *testing.T) {
	runner := &recordingRunner{
		results: map[int]core.LocalCommandResult{
			4: {Stdout: `{"groupVersion":"devbox.sealos.io/v1beta1","resources":[{"name":"devboxes"}]}`},
		},
	}
	doctor, err := (Provider{}).ConfigureDoctor(lifecycleConfig(), core.Runtime{Exec: runner, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	check := findDoctorCheck(t, result.Checks, "crd.devboxes")
	if result.Status != "blocked" || check.Status != "failed" || !strings.Contains(check.Message, "v1alpha2") {
		t.Fatalf("result=%#v check=%#v", result, check)
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
	if isRulesReviewRequest(req) {
		return core.LocalCommandResult{Stdout: `{"status":{"resourceRules":[{"verbs":["*"],"apiGroups":["*"],"resources":["*"]}]}}`}, r.errors[index]
	}
	if isDevboxDiscoveryRequest(req) {
		return core.LocalCommandResult{Stdout: `{"groupVersion":"devbox.sealos.io/v1alpha2","resources":[{"name":"devboxes"}]}`}, r.errors[index]
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
