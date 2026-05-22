package localcontainer

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
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

func recordedArgsForCommand(t *testing.T, runner *recordingRunner, command string) string {
	t.Helper()
	for i := len(runner.calls) - 1; i >= 0; i-- {
		if len(runner.calls[i].Args) > 0 && runner.calls[i].Args[0] == command {
			return strings.Join(runner.calls[i].Args, "\n")
		}
	}
	t.Fatalf("%s command was not recorded: %#v", command, runner.calls)
	return ""
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
		"-e\nCRABBOX_DOCKER_SOCKET=0",
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
	if strings.Contains(args, "-v\n/var/run/docker.sock:/var/run/docker.sock") {
		t.Fatalf("docker socket should be opt-in:\n%s", args)
	}
}

func TestCreateContainerCanMountDockerSocket(t *testing.T) {
	if _, err := os.Stat("/var/run/docker.sock"); err != nil {
		t.Skipf("docker socket not available: %v", err)
	}
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.LocalContainer.DockerSocket = true
	cfg.LocalContainer.WorkRoot = t.TempDir()
	cfg.WorkRoot = cfg.LocalContainer.WorkRoot
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	_, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	for _, want := range []string{
		"--label\ndocker_socket=1",
		"-e\nCRABBOX_DOCKER_SOCKET=1",
		"-v\n" + cfg.LocalContainer.WorkRoot + ":" + cfg.LocalContainer.WorkRoot,
		"-v\n/var/run/docker.sock:/var/run/docker.sock",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("docker socket run args missing %q:\n%s", want, args)
		}
	}
}

func TestCreateContainerMountsDockerHostUnixSocket(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "cbx-sock-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := filepath.Join(socketDir, "docker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+socketPath)
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.LocalContainer.DockerSocket = true
	cfg.LocalContainer.WorkRoot = t.TempDir()
	cfg.WorkRoot = cfg.LocalContainer.WorkRoot
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	_, err = b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	if !strings.Contains(args, "-v\n"+socketPath+":/var/run/docker.sock") {
		t.Fatalf("docker host socket was not mounted:\n%s", args)
	}
	leaseRoot := filepath.Join(cfg.LocalContainer.WorkRoot, "cbx_123")
	info, err := os.Stat(leaseRoot)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o777 {
		t.Fatalf("lease work root mode=%#o want 0777", info.Mode().Perm())
	}
}

func TestDockerSocketMountRejectsRemoteDockerHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	if _, err := b.dockerSocketMountPath(context.Background()); err == nil {
		t.Fatal("remote docker host accepted")
	}
}

func TestConfigForRunUsesHostVisibleWorkRootWithDockerSocket(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.LocalContainer.DockerSocket = true
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	got := b.configForRun()
	if got.LocalContainer.WorkRoot == "/work/crabbox" || got.WorkRoot == "/work/crabbox" {
		t.Fatalf("docker socket work root should be host-visible: %#v", got.LocalContainer)
	}
	if !strings.Contains(got.LocalContainer.WorkRoot, "crabbox") || got.WorkRoot != got.LocalContainer.WorkRoot {
		t.Fatalf("unexpected docker socket work root: workRoot=%q local=%q", got.WorkRoot, got.LocalContainer.WorkRoot)
	}
}

func TestBootstrapScriptUsesAccountHomeDirectory(t *testing.T) {
	for _, want := range []string{
		`home_dir="$(getent passwd "$user" | cut -d: -f6)"`,
		`"$home_dir/.ssh/authorized_keys"`,
		`if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ]; then`,
		`chown -R "$user" "$home_dir/.ssh"`,
		`chown -R "$user" "$home_dir/.ssh" "$work_root"`,
	} {
		if !strings.Contains(bootstrapScript, want) {
			t.Fatalf("bootstrap script missing %q", want)
		}
	}
}

func TestBootstrapScriptSupportsDockerSocketCLI(t *testing.T) {
	for _, want := range []string{
		`[ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ] && ! command -v docker`,
		`apt-get install -y --no-install-recommends docker.io`,
		`docker socket requested but docker CLI is not installed`,
		`stat -c '%g' /var/run/docker.sock`,
		`usermod -aG "$socket_group" "$user"`,
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

func TestReleaseLeaseRemovesStoredKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	keyPath, err := core.TestboxKeyPath("cbx_release")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"rm", "-f", "container123"}): {},
		},
	}
	b := testBackend(runner)
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_release", Server: core.Server{CloudID: "container123"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stored key still exists after release: %v", err)
	}
}
