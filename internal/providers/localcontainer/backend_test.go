package localcontainer

import (
	"context"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	calls     []core.LocalCommandRequest
	responses map[string]core.LocalCommandResult
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if result, ok := r.responses[commandKey(req.Args)]; ok {
		return result, nil
	}
	if len(req.Args) > 0 {
		if result, ok := r.responses[req.Args[0]]; ok {
			return result, nil
		}
	}
	return core.LocalCommandResult{}, nil
}

func commandKey(args []string) string {
	return strings.Join(args, "\x00")
}

func testBackend(runner *recordingRunner) *backend {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.LocalContainer = core.LocalContainerConfig{
		Runtime:  "docker",
		Image:    "ubuntu:24.04",
		User:     "runner",
		WorkRoot: "/workspace/crabbox",
		CPUs:     4,
		Memory:   "8g",
		Network:  "bridge",
	}
	return newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
}

func TestProviderAliases(t *testing.T) {
	for _, name := range []string{"local-container", "docker", "container", "local-docker"} {
		provider, err := core.ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", name, err)
		}
		if provider.Name() != providerName {
			t.Fatalf("ProviderFor(%q).Name=%q", name, provider.Name())
		}
	}
	spec := Provider{}.Spec()
	if !spec.Features.Has(core.FeatureDesktop) || !spec.Features.Has(core.FeatureBrowser) {
		t.Fatalf("local-container features=%v, want desktop and browser", spec.Features)
	}
}

func TestCreateContainerUsesDockerCompatibleSSHLease(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	id, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err != nil {
		t.Fatal(err)
	}
	if id != "container123456" {
		t.Fatalf("id=%q", id)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d", len(runner.calls))
	}
	call := runner.calls[0]
	if call.Name != "docker" {
		t.Fatalf("runtime=%q", call.Name)
	}
	args := strings.Join(call.Args, "\n")
	for _, want := range []string{
		"run",
		"--name\ncrabbox-blue",
		"--user\nroot",
		"--network\nbridge",
		"-p\n127.0.0.1::2222",
		"-e\nCRABBOX_SSH_USER=runner",
		"-e\nCRABBOX_WORK_ROOT=/workspace/crabbox",
		"-e\nCRABBOX_DESKTOP=0",
		"-e\nCRABBOX_BROWSER=0",
		"--cpus\n4",
		"--memory\n8g",
		"--label\nprovider=local-container",
		"--label\nlease=cbx_123",
		"--label\nslug=blue-lobster",
		"--label\nssh_user=runner",
		"--label\nwork_root=/workspace/crabbox",
		"ubuntu:24.04",
		"/bin/sh",
		"-lc",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("docker run args missing %q:\n%s", want, args)
		}
	}
}

func TestBootstrapScriptUsesAccountHomeDirectory(t *testing.T) {
	for _, want := range []string{
		`home_dir="$(getent passwd "$user" | cut -d: -f6)"`,
		`"$home_dir/.ssh/authorized_keys"`,
		`chown -R "$user" "$home_dir/.ssh" "$work_root"`,
	} {
		if !strings.Contains(bootstrapScript, want) {
			t.Fatalf("bootstrap script missing %q", want)
		}
	}
}

func TestConfigForRunHonorsGlobalWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.WorkRoot = "/tmp/cbx"
	cfg.LocalContainer.WorkRoot = ""

	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	got := b.configForRun()
	if got.WorkRoot != "/tmp/cbx" || got.LocalContainer.WorkRoot != "/tmp/cbx" {
		t.Fatalf("work root = %q local=%q, want /tmp/cbx", got.WorkRoot, got.LocalContainer.WorkRoot)
	}
}

func TestApplyDefaultsDoesNotMaskUnsupportedTarget(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	cfg.WindowsMode = "normal"

	applyDefaults(&cfg)
	if cfg.TargetOS != core.TargetWindows || cfg.WindowsMode != "normal" {
		t.Fatalf("target = %s windowsMode=%s, want explicit windows target preserved", cfg.TargetOS, cfg.WindowsMode)
	}
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted unsupported windows target")
	}
}

func TestListAndResolveContainers(t *testing.T) {
	inspectJSON := `[{
		"Id":"abcdef1234567890",
		"Name":"/crabbox-blue",
		"Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_123","slug":"blue-lobster","state":"ready","server_type":"ubuntu:24.04","ssh_user":"runner","work_root":"/workspace/crabbox"}},
		"State":{"Status":"running","Running":true},
		"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}}
	}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "abcdef1234567890\n"},
			commandKey([]string{"inspect", "abcdef1234567890"}): {Stdout: inspectJSON},
		},
	}
	b := testBackend(runner)

	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views=%d", len(views))
	}
	if views[0].Provider != providerName || views[0].CloudID != "abcdef1234567890" || views[0].Labels["ssh_port"] != "49153" {
		t.Fatalf("unexpected view: %#v", views[0])
	}

	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "blue-lobster"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_123" || lease.SSH.Host != "127.0.0.1" || lease.SSH.Port != "49153" || lease.SSH.User != "runner" || lease.SSH.TargetOS != core.TargetLinux || len(lease.SSH.FallbackPorts) != 0 || !strings.Contains(lease.SSH.ReadyCheck, "rsync --version") {
		t.Fatalf("unexpected lease: %#v", lease)
	}
}

func TestFindContainerForClaimReturnsMatchedContainerIdentity(t *testing.T) {
	inspectJSON := `[{
		"Id":"newcontainer123456",
		"Name":"/crabbox-blue-new",
		"Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_new","slug":"blue-lobster","state":"ready","server_type":"ubuntu:24.04","ssh_user":"runner","work_root":"/workspace/crabbox"}},
		"State":{"Status":"running","Running":true},
		"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49154"}]}}
	}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "newcontainer123456\n"},
			commandKey([]string{"inspect", "newcontainer123456"}): {Stdout: inspectJSON},
		},
	}
	b := testBackend(runner)

	container, leaseID, slug, err := b.findContainerForClaim(context.Background(), core.LeaseClaim{LeaseID: "cbx_old", Slug: "blue-lobster"})
	if err != nil {
		t.Fatal(err)
	}
	if container.ID != "newcontainer123456" || leaseID != "cbx_new" || slug != "blue-lobster" {
		t.Fatalf("identity = container=%s lease=%s slug=%s", container.ID, leaseID, slug)
	}
}
