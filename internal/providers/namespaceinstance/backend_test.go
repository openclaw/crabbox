package namespaceinstance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecKeepsNamespaceAliasForDevbox(t *testing.T) {
	provider := Provider{}
	if provider.Name() != providerName {
		t.Fatalf("provider name=%q", provider.Name())
	}
	if strings.Join(provider.Aliases(), ",") != "namespace-compute,nsc" {
		t.Fatalf("aliases=%v", provider.Aliases())
	}
	spec := provider.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	if !spec.Features.Has(core.FeatureSSH) || !spec.Features.Has(core.FeatureCrabboxSync) || !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("features=%v", spec.Features)
	}
}

func TestAcquireBuildsNSCCreateAndReturnsSSHTarget(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	runner := &queuedRunner{results: []runnerResult{{
		stdout: `{"id":"inst-1","name":"crabbox-test","status":"running","machine_type":"8x16","ssh":{"user":"crabbox","host":"203.0.113.10","port":2222},"labels":{"provider":"namespace-instance","lease":"cbx_test","slug":"test"}}`,
	}}}
	var stderr bytes.Buffer
	restoreWait := stubWait()
	defer restoreWait()

	cfg := core.Config{
		Provider:    providerName,
		TargetOS:    core.TargetLinux,
		SSHUser:     "crabbox",
		Class:       "large",
		WorkRoot:    "/workspace/default",
		IdleTimeout: time.Hour,
		NamespaceInstance: core.NamespaceInstanceConfig{
			Duration:  15 * time.Minute,
			Ephemeral: true,
			Region:    "us-west-1",
			Endpoint:  "https://compute.namespace.example",
			Keychain:  "test-keychain",
			Volumes:   []string{"cache:/var/cache"},
			WorkRoot:  "/workspaces/crabbox",
		},
	}
	backend := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: &stderr, Exec: runner}).(*Backend)

	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, RequestedSlug: "test"})
	if err != nil {
		t.Fatalf("Acquire err=%v", err)
	}
	if lease.Server.CloudID != "inst-1" || lease.SSH.Host != "203.0.113.10" || lease.SSH.User != "crabbox" || lease.SSH.Port != "2222" {
		t.Fatalf("lease=%#v", lease)
	}
	if len(runner.calls) != 1 || runner.calls[0].Name != "nsc" {
		t.Fatalf("calls=%#v", runner.calls)
	}
	args := strings.Join(runner.calls[0].Args, "\x00")
	for _, want := range []string{
		"create",
		"--machine_type\x008x16",
		"--duration\x0015m0s",
		"--ephemeral",
		"--ssh_key",
		"--unique_tag",
		"--label\x00provider=namespace-instance",
		"--label\x00slug=test",
		"--region\x00us-west-1",
		"--endpoint\x00https://compute.namespace.example",
		"--keychain\x00test-keychain",
		"--volume\x00cache:/var/cache",
		"-o\x00json",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("create args missing %q: %q", want, args)
		}
	}
}

func TestListFiltersCrabboxOwnedInstances(t *testing.T) {
	runner := &queuedRunner{results: []runnerResult{{
		stdout: `[
{"id":"owned","name":"crabbox-owned","state":"running","machineType":"4x8","ssh_host":"203.0.113.10","labels":{"provider":"namespace-instance","lease":"cbx_owned","slug":"owned"}},
{"id":"other","name":"user-instance","state":"running","machineType":"4x8","labels":{"owner":"someone"}}
]`,
	}}}
	backend := NewBackend(Provider{}.Spec(), core.Config{}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*Backend)

	views, err := backend.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatalf("List err=%v", err)
	}
	if len(views) != 1 || views[0].CloudID != "owned" || views[0].Labels["slug"] != "owned" {
		t.Fatalf("views=%#v", views)
	}
}

func TestDestroyTreatsMissingInstanceAsReleased(t *testing.T) {
	runner := &queuedRunner{results: []runnerResult{{
		stderr: "instance not found",
		err:    errors.New("exit status 1"),
		code:   1,
	}}}
	backend := NewBackend(Provider{}.Spec(), core.Config{}, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*Backend)

	if err := backend.destroyInstance(context.Background(), "missing"); err != nil {
		t.Fatalf("destroyInstance err=%v", err)
	}
	if len(runner.calls) != 1 || strings.Join(runner.calls[0].Args, " ") != "destroy missing --force" {
		t.Fatalf("calls=%#v", runner.calls)
	}
}

func TestLifecycleCommandsUseConfiguredNSCContext(t *testing.T) {
	runner := &queuedRunner{results: []runnerResult{
		{stdout: `{"id":"inst-1","name":"crabbox-test"}`},
		{stdout: `[]`},
		{},
		{},
		{stdout: `{}`},
		{stdout: `[]`},
	}}
	cfg := core.Config{NamespaceInstance: core.NamespaceInstanceConfig{
		Region:   "us-east-1",
		Endpoint: "https://compute.namespace.example",
		Keychain: "ci-keychain",
		Duration: time.Minute,
		WorkRoot: "/workspaces/crabbox",
	}}
	backend := NewBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*Backend)

	if _, err := backend.describeInstance(context.Background(), "inst-1"); err != nil {
		t.Fatalf("describe err=%v", err)
	}
	if _, err := backend.listInstances(context.Background()); err != nil {
		t.Fatalf("list err=%v", err)
	}
	if err := backend.destroyInstance(context.Background(), "inst-1"); err != nil {
		t.Fatalf("destroy err=%v", err)
	}
	if err := backend.extendInstance(context.Background(), "inst-1", time.Minute); err != nil {
		t.Fatalf("extend err=%v", err)
	}
	if _, err := backend.Doctor(context.Background(), core.DoctorRequest{}); err != nil {
		t.Fatalf("doctor err=%v", err)
	}

	for _, call := range runner.calls {
		args := strings.Join(call.Args, "\x00")
		for _, want := range []string{
			"--region\x00us-east-1",
			"--endpoint\x00https://compute.namespace.example",
			"--keychain\x00ci-keychain",
		} {
			if !strings.Contains(args, want) {
				t.Fatalf("%s args missing %q: %q", call.Args[0], want, args)
			}
		}
	}
}

func TestProviderRejectsUnsafeWorkRoot(t *testing.T) {
	for _, workRoot := range []string{"/", "/workspaces", "/tmp", "relative"} {
		cfg := core.Config{NamespaceInstance: core.NamespaceInstanceConfig{WorkRoot: workRoot}}
		if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
			t.Fatalf("expected %q to be rejected", workRoot)
		}
	}
	cfg := core.Config{NamespaceInstance: core.NamespaceInstanceConfig{WorkRoot: "/workspaces/crabbox"}}
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err != nil {
		t.Fatalf("expected safe workRoot, got %v", err)
	}
}

type runnerResult struct {
	stdout string
	stderr string
	err    error
	code   int
}

type queuedRunner struct {
	calls   []core.LocalCommandRequest
	results []runnerResult
}

func (r *queuedRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if len(r.results) == 0 {
		return core.LocalCommandResult{}, nil
	}
	result := r.results[0]
	r.results = r.results[1:]
	code := result.code
	if code == 0 && result.err != nil {
		code = 1
	}
	return core.LocalCommandResult{Stdout: result.stdout, Stderr: result.stderr, ExitCode: code}, result.err
}

func stubWait() func() {
	previous := namespaceInstanceWaitForSSH
	namespaceInstanceWaitForSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		return nil
	}
	return func() { namespaceInstanceWaitForSSH = previous }
}
