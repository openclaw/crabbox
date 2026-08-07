package runcloud

import (
	"context"
	"errors"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndRegistration(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName || len(p.Aliases()) != 0 {
		t.Fatalf("provider=%q aliases=%v", p.Name(), p.Aliases())
	}
	registered, err := core.ProviderFor(providerName)
	if err != nil {
		t.Fatal(err)
	}
	if registered.Name() != providerName {
		t.Fatalf("registered provider=%q", registered.Name())
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("spec=%#v", spec)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v", spec.Targets)
	}
	if !containsFeature(spec.Features, core.FeatureSSH) || !containsFeature(spec.Features, core.FeatureCrabboxSync) {
		t.Fatalf("features=%#v", spec.Features)
	}
}

func TestProviderFlagsAndWorkdirValidation(t *testing.T) {
	cfg := Config{Provider: providerName, TargetOS: targetLinux, RunCloud: RunCloudConfig{
		CLIPath: "runcloud", Image: "runcloud/agent-base", Workdir: "/home/runcloud/crabbox",
	}}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterRunCloudProviderFlags(fs, cfg)
	if err := fs.Parse([]string{
		"--run-cloud-cli", "/opt/runcloud",
		"--run-cloud-image", "ubuntu:24.04",
		"--run-cloud-region", "eu-north",
		"--run-cloud-workdir", "/workspace/project",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyRunCloudProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.RunCloud.CLIPath != "/opt/runcloud" || cfg.RunCloud.Image != "ubuntu:24.04" || cfg.RunCloud.Region != "eu-north" || cfg.WorkRoot != "/workspace/project" {
		t.Fatalf("cfg=%#v", cfg.RunCloud)
	}
	for _, value := range []string{"", "relative", "/", "/workspace"} {
		if _, err := cleanWorkdir(value); err == nil {
			t.Fatalf("expected workdir %q to fail", value)
		}
	}
}

func TestClientUsesRunCloudCLI(t *testing.T) {
	runner := &recordingRunner{}
	client := &client{cliPath: "/opt/runcloud", runner: runner, pollInterval: time.Millisecond}
	ctx := context.Background()
	if err := client.Check(ctx); err != nil {
		t.Fatal(err)
	}
	sandbox, err := client.CreateSandbox(ctx, createRequest{
		Name: "crabbox-blue-lobster-12345678", Image: "runcloud/agent-base", Region: "eu-north", TTL: 10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.ID != "sbx_1" {
		t.Fatalf("sandbox=%#v", sandbox)
	}
	if _, err := client.GetSandbox(ctx, sandbox.ID); err != nil {
		t.Fatal(err)
	}
	if listed, err := client.ListSandboxes(ctx); err != nil || len(listed) != 1 {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	if _, err := client.ExposeSandbox(ctx, sandbox.ID, sandbox.Name); err != nil {
		t.Fatal(err)
	}
	if err := client.InstallSSHKey(ctx, sandbox.ID, "ssh-ed25519 AAAATEST crabbox"); err != nil {
		t.Fatal(err)
	}
	if err := client.ResumeSandbox(ctx, sandbox.ID); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteSandbox(ctx, sandbox.ID); err != nil {
		t.Fatal(err)
	}

	wantPrefix := [][]string{
		{"account", "--json"},
		{"sandbox", "create", "--name", "crabbox-blue-lobster-12345678", "--image", "runcloud/agent-base", "--cpu", "2", "--memory", "24576", "--persistent", "--region", "eu-north", "--timeout", "600", "--json"},
		{"sandbox", "get", "sbx_1", "--json"},
		{"sandbox", "list", "--json"},
		{"sandbox", "expose", "sbx_1", "--name", "crabbox-blue-lobster-12345678", "--port", "22", "--json"},
	}
	if !reflect.DeepEqual(runner.commands[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("commands=%v", runner.commands)
	}
	bootstrap := runner.commands[len(wantPrefix)]
	if len(bootstrap) != 5 || bootstrap[0] != "sandbox" || bootstrap[1] != "exec" || bootstrap[2] != "sbx_1" || !strings.Contains(bootstrap[3], "useradd --create-home") || !strings.Contains(bootstrap[3], "authorized_keys") || !strings.Contains(bootstrap[3], "apt-get install") || bootstrap[4] != "--json" {
		t.Fatalf("bootstrap=%v", bootstrap)
	}
}

func TestAcquireOnAcquiredErrorRollsBackEvenWhenKept(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{}
	b := &backend{
		spec: Provider{}.Spec(),
		cfg: Config{
			Provider: providerName, TargetOS: targetLinux, Network: networkPublic,
			WorkRoot: "/home/runcloud/crabbox",
			RunCloud: RunCloudConfig{CLIPath: "runcloud", Image: "runcloud/agent-base", Workdir: "/home/runcloud/crabbox"},
		},
		rt:     Runtime{Stdout: io.Discard, Stderr: io.Discard},
		client: fake,
	}
	want := errors.New("reject acquired identity")
	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo: core.Repo{Root: t.TempDir()}, Keep: true,
		OnAcquired: func(LeaseTarget) error { return want },
	})
	if !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(fake.deleted, []string{"sbx_1"}) {
		t.Fatalf("deleted=%v", fake.deleted)
	}
}

func TestDeleteTreatsNotFoundAsSuccess(t *testing.T) {
	runner := &recordingRunner{deleteMissing: true}
	client := &client{cliPath: "runcloud", runner: runner}
	if err := client.DeleteSandbox(context.Background(), "sbx_missing"); err != nil {
		t.Fatal(err)
	}
}

func containsFeature(features core.FeatureSet, feature core.Feature) bool {
	for _, candidate := range features {
		if candidate == feature {
			return true
		}
	}
	return false
}

type recordingRunner struct {
	commands      [][]string
	deleteMissing bool
}

func (r *recordingRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.commands = append(r.commands, append([]string(nil), req.Args...))
	joined := strings.Join(req.Args, " ")
	switch {
	case joined == "account --json":
		return LocalCommandResult{Stdout: `{"products":["sandboxes"]}`}, nil
	case strings.HasPrefix(joined, "sandbox create "):
		return LocalCommandResult{Stdout: `{"id":"sbx_1","name":"crabbox-blue-lobster-12345678","state":"running","image":"runcloud/agent-base"}`}, nil
	case joined == "sandbox get sbx_1 --json":
		return LocalCommandResult{Stdout: `{"id":"sbx_1","name":"crabbox-blue-lobster-12345678","state":"running","box":{"id":"box_1","hostname":"crabbox-box.run.cloud","port":22}}`}, nil
	case joined == "sandbox list --json":
		return LocalCommandResult{Stdout: `[{"id":"sbx_1","name":"crabbox-blue-lobster-12345678","state":"running","hostname":"crabbox-box.run.cloud"}]`}, nil
	case strings.HasPrefix(joined, "sandbox expose "):
		return LocalCommandResult{Stdout: `{"id":"box_1","sandboxId":"sbx_1","name":"crabbox-blue-lobster-12345678","hostname":"crabbox-box.run.cloud","port":22,"status":"ready"}`}, nil
	case strings.HasPrefix(joined, "sandbox exec "):
		return LocalCommandResult{Stdout: `{"exit_code":0,"stdout":"","stderr":""}`}, nil
	case joined == "sandbox resume sbx_1 --json":
		return LocalCommandResult{Stdout: `{"id":"sbx_1","state":"running"}`}, nil
	case joined == "sandbox rm sbx_1 --json":
		return LocalCommandResult{}, nil
	case joined == "sandbox rm sbx_missing --json" && r.deleteMissing:
		return LocalCommandResult{ExitCode: 1, Stderr: "sandbox not found (404)"}, errors.New("exit status 1")
	default:
		return LocalCommandResult{ExitCode: 1, Stderr: "unexpected command: " + joined}, errors.New("unexpected command")
	}
}

type fakeAPI struct {
	installedKey string
	deleted      []string
}

func (*fakeAPI) Check(context.Context) error { return nil }

func (*fakeAPI) CreateSandbox(context.Context, createRequest) (sandboxData, error) {
	return sandboxData{ID: "sbx_1", Name: "crabbox-blue-lobster-12345678", State: "running", Image: "runcloud/agent-base"}, nil
}

func (*fakeAPI) GetSandbox(context.Context, string) (sandboxData, error) {
	return sandboxData{ID: "sbx_1", Name: "crabbox-blue-lobster-12345678", State: "running", Box: &boxData{ID: "box_1", Hostname: "crabbox-box.run.cloud", Port: 22}}, nil
}

func (*fakeAPI) ListSandboxes(context.Context) ([]sandboxData, error) {
	return []sandboxData{{
		ID: "sbx_1", Name: "crabbox-blue-lobster-12345678", State: "running",
		Box: &boxData{ID: "box_1", Hostname: "crabbox-box.run.cloud", Port: 22},
	}}, nil
}

func (*fakeAPI) ExposeSandbox(context.Context, string, string) (boxData, error) {
	return boxData{ID: "box_1", SandboxID: "sbx_1", Hostname: "crabbox-box.run.cloud", Port: 22, Status: "ready"}, nil
}

func (f *fakeAPI) InstallSSHKey(_ context.Context, _ string, key string) error {
	f.installedKey = key
	return nil
}

func (*fakeAPI) ResumeSandbox(context.Context, string) error { return nil }

func (f *fakeAPI) DeleteSandbox(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}
