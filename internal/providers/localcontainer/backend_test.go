package localcontainer

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	calls     []core.LocalCommandRequest
	responses map[string]core.LocalCommandResult
	run       func(core.LocalCommandRequest) (core.LocalCommandResult, error)
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.run != nil {
		return r.run(req)
	}
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

func listenUnixSocketOrSkip(t *testing.T, path string) net.Listener {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "invalid argument") {
			t.Skipf("unix sockets are not permitted in this environment: %v", err)
		}
		t.Fatal(err)
	}
	return listener
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
	if !spec.Features.Has(core.FeatureCleanup) {
		t.Fatalf("local-container features=%v, want cleanup", spec.Features)
	}
	if !spec.Features.Has(core.FeatureCacheVolume) {
		t.Fatalf("local-container features=%v, want cache-volume", spec.Features)
	}
}

func TestCreateContainerUsesDockerCompatibleSSHLease(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "docker"))
	t.Setenv("PATH", dir)
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	id, _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
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
		":/tmp/crabbox-bootstrap:ro",
		"ubuntu:24.04",
		"/bin/sh\n/tmp/crabbox-bootstrap/bootstrap.sh",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("docker run args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "-v\n/var/run/docker.sock:/var/run/docker.sock") {
		t.Fatalf("docker socket should be opt-in:\n%s", args)
	}
}

func TestCreateContainerRemovesBootstrapDirOnRunFailure(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{Stderr: "run failed"}, errors.New("run failed")
		case "ps":
			return core.LocalCommandResult{Stdout: "created123\n"}, nil
		case "inspect":
			return core.LocalCommandResult{Stdout: `[{"Id":"created123","Config":{"Labels":{"lease":"cbx_123","bootstrap_dir":` + strconv.Quote(bootstrapDir) + `}}}]`}, nil
		case "rm":
			if _, err := os.Stat(bootstrapDir); err != nil {
				t.Fatalf("bootstrap directory removed before container rollback: %v", err)
			}
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	if _, _, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", false); err == nil {
		t.Fatal("createContainer succeeded")
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap directory still exists after run failure: %v", err)
	}
	if args := recordedArgsForCommand(t, runner, "rm"); !strings.Contains(args, "created123") {
		t.Fatalf("run failure rollback did not remove owned container:\n%s", args)
	}
}

func TestCreateContainerPreservesBootstrapDirWhenRollbackFails(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{Stderr: "run failed"}, errors.New("run failed")
		case "ps":
			return core.LocalCommandResult{Stdout: "created123\n"}, nil
		case "inspect":
			return core.LocalCommandResult{Stdout: `[{"Id":"created123","Config":{"Labels":{"lease":"cbx_123","bootstrap_dir":` + strconv.Quote(bootstrapDir) + `}}}]`}, nil
		case "rm":
			return core.LocalCommandResult{Stderr: "daemon unavailable"}, errors.New("remove failed")
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	if _, _, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", false); err == nil {
		t.Fatal("createContainer succeeded")
	}
	if _, err := os.Stat(bootstrapDir); err != nil {
		t.Fatalf("bootstrap directory missing after failed container rollback: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
}

func TestCreateContainerRunFailurePreservesUnownedContainer(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{Stderr: "name conflict"}, errors.New("run failed")
		case "ps":
			return core.LocalCommandResult{}, nil
		case "rm":
			t.Fatal("removed unowned container after run failure")
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	if _, _, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", false); err == nil {
		t.Fatal("createContainer succeeded")
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap directory still exists after run failure: %v", err)
	}
}

func TestCreateContainerRunFailureKeepsOwnedContainer(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{Stderr: "connection lost"}, errors.New("run failed")
		case "ps":
			return core.LocalCommandResult{Stdout: "created123\n"}, nil
		case "inspect":
			return core.LocalCommandResult{Stdout: `[{"Id":"created123","Config":{"Labels":{"lease":"cbx_123","bootstrap_dir":` + strconv.Quote(bootstrapDir) + `}}}]`}, nil
		case "rm":
			t.Fatal("removed kept container after run failure")
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	containerID, _, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err == nil {
		t.Fatal("createContainer succeeded")
	}
	if containerID != "created123" {
		t.Fatalf("kept container id = %q, want created123", containerID)
	}
	if _, err := os.Stat(bootstrapDir); err != nil {
		t.Fatalf("kept bootstrap directory missing after run failure: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
}

func TestCreateContainerRunFailureKeepsBootstrapDirWhenOwnershipUnknown(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{Stderr: "connection lost"}, errors.New("run failed")
		case "ps":
			return core.LocalCommandResult{Stderr: "daemon unavailable"}, errors.New("inspect failed")
		case "rm":
			t.Fatal("removed container with unknown ownership after run failure")
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	if _, _, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true); err == nil {
		t.Fatal("createContainer succeeded")
	}
	if _, err := os.Stat(bootstrapDir); err != nil {
		t.Fatalf("bootstrap directory missing with unknown container ownership: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
}

func TestAcquireRunFailureKeepsOwnedContainerKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var leaseID string
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			leaseID = labelFromRunArgs(t, req.Args, "lease")
			return core.LocalCommandResult{Stderr: "connection lost"}, errors.New("run failed")
		case "ps":
			if strings.Contains(strings.Join(req.Args, "\n"), "label=lease=") {
				return core.LocalCommandResult{Stdout: "created123\n"}, nil
			}
			return core.LocalCommandResult{}, nil
		case "inspect":
			return core.LocalCommandResult{Stdout: `[{"Id":"created123","Config":{"Labels":{"lease":` + strconv.Quote(leaseID) + `,"bootstrap_dir":` + strconv.Quote(bootstrapDir) + `}}}]`}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	if _, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}}); err == nil {
		t.Fatal("Acquire succeeded")
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("kept container SSH key missing: %v", err)
	}
	t.Cleanup(func() {
		core.RemoveStoredTestboxKey(leaseID)
		_ = os.RemoveAll(bootstrapDir)
	})
}

func TestAcquirePostCreateFailureKeepsRetainedContainerKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var leaseID string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			leaseID = labelFromRunArgs(t, req.Args, "lease")
			return core.LocalCommandResult{Stdout: "created123\n"}, nil
		case "inspect":
			return core.LocalCommandResult{Stderr: "inspect failed"}, errors.New("inspect failed")
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	if _, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}}); err == nil {
		t.Fatal("Acquire succeeded")
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("kept post-create container SSH key missing: %v", err)
	}
	t.Cleanup(func() {
		core.RemoveStoredTestboxKey(leaseID)
	})
}

func TestCreateContainerRecoversEmptyContainerID(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{}, nil
		case "ps":
			return core.LocalCommandResult{Stdout: "created123\n"}, nil
		case "inspect":
			return core.LocalCommandResult{Stdout: `[{"Id":"created123","Config":{"Labels":{"lease":"cbx_123","bootstrap_dir":` + strconv.Quote(bootstrapDir) + `}}}]`}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	containerID, gotBootstrapDir, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", false)
	if err != nil {
		t.Fatal(err)
	}
	if containerID != "created123" {
		t.Fatalf("container id = %q, want created123", containerID)
	}
	if gotBootstrapDir != bootstrapDir {
		t.Fatalf("bootstrap directory = %q, want %q", gotBootstrapDir, bootstrapDir)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
}

func TestCreateContainerRemovesBootstrapDirWhenEmptyContainerIDIsNotFound(t *testing.T) {
	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{}, nil
		case "ps":
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	if _, _, err := b.createContainer(context.Background(), b.configForRun(), "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true); err == nil {
		t.Fatal("createContainer succeeded")
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap directory still exists without a container: %v", err)
	}
}

func TestAcquireRollbackRemovesContainerBeforeBootstrapDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var bootstrapDir string
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		switch firstArg(req.Args) {
		case "ps":
			return core.LocalCommandResult{}, nil
		case "run":
			bootstrapDir = bootstrapDirFromRunArgs(t, []core.LocalCommandRequest{req})
			return core.LocalCommandResult{Stdout: "container123456\n"}, nil
		case "inspect":
			return core.LocalCommandResult{Stdout: "{"}, nil
		case "rm":
			if _, err := os.Stat(bootstrapDir); err != nil {
				t.Fatalf("bootstrap directory removed before container rollback: %v", err)
			}
			return core.LocalCommandResult{}, nil
		default:
			return core.LocalCommandResult{}, nil
		}
	}
	b := testBackend(runner)

	if _, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}}); err == nil {
		t.Fatal("Acquire succeeded")
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap directory still exists after acquire rollback: %v", err)
	}
	if args := recordedArgsForCommand(t, runner, "rm"); !strings.Contains(args, "container123456") {
		t.Fatalf("acquire rollback did not remove container:\n%s", args)
	}
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func bootstrapDirFromRunArgs(t *testing.T, calls []core.LocalCommandRequest) string {
	t.Helper()
	for _, call := range calls {
		for i := 0; i+1 < len(call.Args); i++ {
			if call.Args[i] != "--label" || !strings.HasPrefix(call.Args[i+1], "bootstrap_dir=") {
				continue
			}
			return strings.TrimPrefix(call.Args[i+1], "bootstrap_dir=")
		}
	}
	t.Fatal("bootstrap_dir label not found in docker run args")
	return ""
}

func labelFromRunArgs(t *testing.T, args []string, key string) string {
	t.Helper()
	prefix := key + "="
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--label" && strings.HasPrefix(args[i+1], prefix) {
			return strings.TrimPrefix(args[i+1], prefix)
		}
	}
	t.Fatalf("%s label not found in docker run args", key)
	return ""
}

func TestConfigForRunFallsBackToPodmanWhenDockerIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "podman"))
	t.Setenv("PATH", dir)
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	got := b.configForRun()
	if got.LocalContainer.Runtime != "podman" {
		t.Fatalf("runtime=%q, want podman", got.LocalContainer.Runtime)
	}
}

func TestConfigForRunPrefersDockerWhenBothRuntimesExist(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "docker"))
	writeExecutable(t, filepath.Join(dir, "podman"))
	t.Setenv("PATH", dir)
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	got := b.configForRun()
	if got.LocalContainer.Runtime != "docker" {
		t.Fatalf("runtime=%q, want docker", got.LocalContainer.Runtime)
	}
}

func TestConfigForRunHonorsExplicitRuntime(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "docker"))
	t.Setenv("PATH", dir)
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	b.cfg.LocalContainer.Runtime = "podman"
	core.MarkLocalContainerRuntimeExplicit(&b.cfg)
	got := b.configForRun()
	if got.LocalContainer.Runtime != "podman" {
		t.Fatalf("runtime=%q, want explicit podman", got.LocalContainer.Runtime)
	}
}

func TestConfigForRunHonorsExplicitDockerRuntime(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "podman"))
	t.Setenv("PATH", dir)
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	b.cfg.LocalContainer.Runtime = "docker"
	core.MarkLocalContainerRuntimeExplicit(&b.cfg)
	got := b.configForRun()
	if got.LocalContainer.Runtime != "docker" {
		t.Fatalf("runtime=%q, want explicit docker", got.LocalContainer.Runtime)
	}
}

func TestClaimScopeSkipsDockerContextForPodman(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	b.cfg.LocalContainer.Runtime = "podman"

	scope := b.claimScope(context.Background())
	if scope != "runtime:podman/context:default" {
		t.Fatalf("scope=%q, want podman default scope", scope)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "context" {
			t.Fatalf("podman claim scope should not call context command: %#v", call.Args)
		}
	}
}

func TestRuntimeInfoSkipsDockerContextForPodman(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"version", "--format", "{{.Client.Version}}"}): {Stdout: "5.8.2\n"},
		},
	}
	b := testBackend(runner)
	b.cfg.LocalContainer.Runtime = "podman"

	version, contextName := b.runtimeInfo(context.Background())
	if version != "5.8.2" || contextName != "default" {
		t.Fatalf("version=%q context=%q", version, contextName)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "context" {
			t.Fatalf("podman runtime info should not call context command: %#v", call.Args)
		}
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestCreateContainerPassesDesktopEnv(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.Desktop = true
	cfg.DesktopEnv = "wayland"
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	if _, _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	for _, want := range []string{
		"-e\nCRABBOX_DESKTOP=1",
		"-e\nCRABBOX_DESKTOP_ENV=wayland",
		"--label\ndesktop=true",
		"--label\ndesktop_env=wayland",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("docker run args missing %q:\n%s", want, args)
		}
	}
}

func TestCreateContainerMountsCacheVolumes(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.Cache.Volumes = []core.CacheVolumeConfig{
		{Key: "my-app/linux node24 lock", Path: "/var/cache/crabbox/pnpm"},
		{Key: "npm-cache", Path: "/var/cache/crabbox/npm"},
	}
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	if _, _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	for _, volume := range cfg.Cache.Volumes {
		want := "-v\n" + localContainerCacheVolumeName(volume.Key) + ":" + volume.Path
		if !strings.Contains(args, want) {
			t.Fatalf("cache volume mount missing %q:\n%s", want, args)
		}
	}
	for i, volume := range cfg.Cache.Volumes {
		want := "-e\nCRABBOX_CACHE_VOLUME_PATH_" + strconv.Itoa(i) + "=" + volume.Path
		if !strings.Contains(args, want) {
			t.Fatalf("cache volume path env missing %q:\n%s", want, args)
		}
	}
}

func TestLocalContainerCacheVolumeNameIsStableAndDockerSafe(t *testing.T) {
	got := localContainerCacheVolumeName("My App/linux node24 lock")
	again := localContainerCacheVolumeName("My App/linux node24 lock")
	if got != again {
		t.Fatalf("cache volume name unstable: %q then %q", got, again)
	}
	if !strings.HasPrefix(got, "crabbox-cache-my-app-linux-node24-lock-") {
		t.Fatalf("cache volume name=%q, want sanitized prefix", got)
	}
	if strings.ContainsAny(got, " /:") {
		t.Fatalf("cache volume name contains unsafe characters: %q", got)
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

	_, _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	for _, want := range []string{
		"--label\ndocker_socket=1",
		"--label\nhost_work_root=" + cfg.LocalContainer.WorkRoot,
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
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "docker.sock")
	listener := listenUnixSocketOrSkip(t, socketPath)
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
	rootInfo, err := os.Stat(cfg.LocalContainer.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	rootMode := rootInfo.Mode().Perm()
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	_, _, err = b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	wantSocketPath := socketPath
	if runtime.GOOS != "linux" {
		wantSocketPath = "/var/run/docker.sock"
	}
	if !strings.Contains(args, "-v\n"+wantSocketPath+":/var/run/docker.sock") {
		t.Fatalf("docker host socket was not mounted:\n%s", args)
	}
	rootInfo, err = os.Stat(cfg.LocalContainer.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != rootMode {
		t.Fatalf("host work root mode=%#o want preserved %#o", rootInfo.Mode().Perm(), rootMode)
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

func TestCreateContainerMountsPodmanSocketWithSecurityOpt(t *testing.T) {
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "podman.sock")
	listener := listenUnixSocketOrSkip(t, socketPath)
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+socketPath)
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{},
	}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.LocalContainer.Runtime = "podman"
	cfg.LocalContainer.DockerSocket = true
	cfg.LocalContainer.WorkRoot = t.TempDir()
	cfg.WorkRoot = cfg.LocalContainer.WorkRoot
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "container123456\n"}

	_, _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	wantSocketPath := socketPath
	if runtime.GOOS != "linux" {
		wantSocketPath = "/var/run/docker.sock"
	}
	for _, want := range []string{
		"-v\n" + wantSocketPath + ":/var/run/docker.sock",
		"--security-opt\nlabel=disable",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("podman socket run args missing %q:\n%s", want, args)
		}
	}
}

func TestDockerSocketMountRejectsRemoteDockerHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	if _, err := b.dockerSocketMountPath(context.Background()); err == nil {
		t.Fatal("remote docker host accepted")
	}
}

func TestDockerSocketMountUsesDaemonSocketForNonLinuxClient(t *testing.T) {
	path, err := dockerSocketMountPathFromHostForGOOS("unix:///var/run/docker.sock", "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/var/run/docker.sock" {
		t.Fatalf("path=%q, want daemon-visible socket", path)
	}
}

func TestDockerSocketMountUsesDaemonSocketForWindowsPipe(t *testing.T) {
	path, err := dockerSocketMountPathFromHostForGOOS(`npipe:////./pipe/docker_engine`, "windows")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/var/run/docker.sock" {
		t.Fatalf("path=%q, want daemon-visible socket", path)
	}
}

func TestDockerSocketWorkRootsUseLinuxGuestPathForWindows(t *testing.T) {
	host, guest := dockerSocketWorkRootsForGOOS(`C:\crabbox\local-container-work`, "windows")
	if host != `C:\crabbox\local-container-work` {
		t.Fatalf("host root=%q", host)
	}
	if guest != "/work/crabbox" {
		t.Fatalf("guest root=%q, want /work/crabbox", guest)
	}

	host, guest = dockerSocketWorkRootsForGOOS("/work/custom", "windows")
	if !strings.Contains(host, "crabbox") || guest != "/work/custom" {
		t.Fatalf("default Windows socket roots host=%q guest=%q", host, guest)
	}
}

func TestPrepareLeaseUsesLabeledGuestWorkRootForReadyCheck(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	cfg := b.configForRun()
	cfg.LocalContainer.DockerSocket = true
	cfg.LocalContainer.WorkRoot = `C:\crabbox\local-container-work`
	cfg.WorkRoot = cfg.LocalContainer.WorkRoot
	container := inspectContainer{
		ID:   "container1234567890",
		Name: "/crabbox-windows-root",
		Config: inspectConfig{
			Image: "ubuntu:24.04",
			Labels: map[string]string{
				"crabbox":        "true",
				"provider":       providerName,
				"lease":          "cbx_windows",
				"slug":           "windows-root",
				"state":          "ready",
				"server_type":    "ubuntu:24.04",
				"ssh_user":       "runner",
				"docker_socket":  "1",
				"host_work_root": `C:\crabbox\local-container-work`,
				"work_root":      "/work/crabbox",
			},
		},
		State: inspectState{Status: "running", Running: true},
		NetworkSettings: inspectNetworking{
			Ports: map[string][]inspectPort{"2222/tcp": []inspectPort{{HostIP: "127.0.0.1", HostPort: "49153"}}},
		},
	}

	lease, err := b.prepareLease(context.Background(), cfg, container, "cbx_windows", "windows-root", false)
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.ReadyCheck == "" || !strings.Contains(lease.SSH.ReadyCheck, "test -d '/work/crabbox'") {
		t.Fatalf("ready check did not use guest work root: %q", lease.SSH.ReadyCheck)
	}
	if strings.Contains(lease.SSH.ReadyCheck, `C:\crabbox`) {
		t.Fatalf("ready check used host work root: %q", lease.SSH.ReadyCheck)
	}
	if lease.SSH.User != "runner" || lease.SSH.Port != "49153" {
		t.Fatalf("unexpected lease target: %#v", lease.SSH)
	}
}

func TestConfigForRunUsesHostVisibleWorkRootWithDockerSocket(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.LocalContainer.DockerSocket = true
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	got := b.configForRun()
	if runtime.GOOS == "windows" {
		if got.LocalContainer.WorkRoot != "/work/crabbox" || got.WorkRoot != "/work/crabbox" {
			t.Fatalf("windows docker socket work root should stay Linux-visible: %#v", got.LocalContainer)
		}
		return
	}
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
		`sed -i 's/^[#[:space:]]*UsePAM[[:space:]].*/UsePAM no/' /etc/ssh/sshd_config`,
		`printf '\nUsePAM no\n' >> /etc/ssh/sshd_config`,
		`sed -i 's/^[#[:space:]]*PasswordAuthentication[[:space:]].*/PasswordAuthentication no/' /etc/ssh/sshd_config`,
		`passwd -d "$user" >/dev/null 2>&1 || true`,
		`if [ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ]; then`,
		`chown -R "$user" "$home_dir/.ssh"`,
		`chown -R "$user" "$home_dir/.ssh" "$work_root"`,
		`arc-theme`,
		`"$config_dir/xfce4/xfconf/xfce-perchannel-xml/xsettings.xml"`,
		`mode="${CRABBOX_DESKTOP_THEME:-}"`,
		`"$config_dir/crabbox/desktop-theme"`,
		`gtk_theme=Adwaita-dark`,
		`gtk_candidates="Arc-Dark Greybird-dark Adwaita-dark Greybird"`,
		`gtk_candidates="Arc Greybird Adwaita"`,
		`xfwm_theme=Default`,
		`xfwm_candidates="Arc-Dark Greybird-dark Daloa Default"`,
		`xfwm_candidates="Arc Greybird Daloa Default"`,
		`ThemeName" type="string" value="$gtk_theme"`,
		`"$config_dir/xfce4/xfconf/xfce-perchannel-xml/xfwm4.xml"`,
		`theme" type="string" value="$xfwm_theme"`,
		`gtk-application-prefer-dark-theme=$gtk_prefer_dark_ini`,
		`xfconf-query -c xsettings -p /Gtk/ApplicationPreferDarkTheme`,
		`xfconf-query -c xfwm4 -p /general/theme`,
		`xfconf-query -c xfwm4 -p /general/box_move`,
		`xfconf-query -c xfwm4 -p /general/box_resize`,
		`xfconf-query -c xfwm4 -p /general/move_opacity`,
		`xfconf-query -c xfwm4 -p /general/resize_opacity`,
		`xfconf-query -c xfwm4 -p /general/snap_to_border`,
		`xfconf-query -c xfwm4 -p /general/snap_width`,
		`xfconf-query -c xfwm4 -p /general/tile_on_move`,
		`xfconf-query -c xfwm4 -p /general/use_compositing`,
		`xfconf-query -c xfwm4 -p /general/wrap_windows`,
		`xfconf-query -c xfce4-panel -p /panels/dark-mode`,
		`/panels/$panel_id/background-rgba`,
		`crabbox desktop theme start`,
		`crabbox-xfce4-panel-$user.log`,
		`pkill -TERM -x xfce4-panel`,
		`xfwm4 --replace --compositor=off`,
		`-wait 16 -defer 8 -nowait_bog`,
		`wayvnc --config '$home_dir/.config/wayvnc/config' --render-cursor --max-fps=60`,
		`gsettings set org.gnome.desktop.interface color-scheme '$gsettings_scheme'`,
		`if [ "$(id -u)" -eq 0 ]; then`,
		`mkdir -p "$config_dir/crabbox" "$config_dir/gtk-3.0" "$config_dir/gtk-4.0" "$config_dir/labwc"`,
		`dbus_address="${DBUS_SESSION_BUS_ADDRESS:-}"`,
		`DBUS_SESSION_BUS_ADDRESS='$dbus_address' GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme`,
		`DISPLAY="$display" XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$dbus_address" GDK_BACKEND=x11 gsettings set org.gnome.desktop.interface color-scheme "$gsettings_scheme"`,
		`"$config_dir/labwc/themerc-override"`,
		`window.active.title.bg.color`,
		`window.active.button.unpressed.image.color`,
		`LABWC_PID="$labwc_pid"`,
		`labwc --reconfigure`,
		`kill -HUP "$labwc_pid"`,
		`"$config_dir/gtk-3.0/gtk.css"`,
		`menubar menuitem`,
		`desktop-background-$mode.svg`,
		`swaybg -i "$wallpaper_file" -m fill`,
		`nohup gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &`,
		`elif [ "$(id -u)" -ne 0 ] && pgrep -x gnome-panel`,
	} {
		if !strings.Contains(bootstrapScript, want) {
			t.Fatalf("bootstrap script missing %q", want)
		}
	}
}

func TestBootstrapScriptSupportsWaylandDesktop(t *testing.T) {
	for _, want := range []string{
		`CRABBOX_DESKTOP_ENV:-xfce`,
		`labwc wayvnc foot grim slurp wtype wl-clipboard wlr-randr`,
		`xdg-desktop-portal-wlr`,
		`CRABBOX_DESKTOP_ENV=$desktop_env`,
		`.config/labwc/autostart`,
		`wlr-randr --output HEADLESS-1 --custom-mode 1920x1080`,
		`foot --title='Crabbox Desktop' >/tmp/crabbox-foot.log 2>&1 &`,
		`for socket in "$runtime"/wayland-*`,
		`display="${socket##*/}"`,
		`desktop_env="${CRABBOX_DESKTOP_ENV:-wayland}"`,
		`CRABBOX_DESKTOP_ENV='$desktop_env'`,
		`labwc wayvnc swaybg librsvg2-common gnome-panel wlr-randr grim slurp wtype wl-clipboard`,
		`swaybg librsvg2-common`,
		`gnome-terminal nautilus gsettings-desktop-schemas adwaita-icon-theme`,
		`DISPLAY=:0`,
		`export GDK_BACKEND=x11`,
		`export MOZ_ENABLE_WAYLAND=0`,
		`gnome-panel >/tmp/crabbox-gnome-panel.log 2>&1 &`,
		`gnome-terminal -- bash -l`,
		`nautilus --new-window "$HOME"`,
		`--user-data-dir=`,
		`if [ "$desktop_env" = "gnome" ]; then
    cat >/usr/local/bin/crabbox-configure-desktop-theme`,
		`crabbox-configure-desktop-theme`,
		`desktop-theme`,
		`gsettings set org.gnome.desktop.interface color-scheme '$gsettings_scheme'`,
		`--force-dark-mode --enable-features=WebUIDarkMode --blink-settings=preferredColorScheme=2`,
		`--blink-settings=preferredColorScheme=1`,
		`WLR_BACKENDS=headless`,
		`rm -f /var/lib/crabbox/display.env`,
		`dbus-run-session labwc`,
		`/tmp/crabbox-labwc.log`,
		`wayvnc --config`,
		`--ozone-platform=x11`,
		`--ozone-platform=wayland`,
	} {
		if !strings.Contains(bootstrapScript, want) {
			t.Fatalf("bootstrap script missing %q", want)
		}
	}
	for _, notWant := range []string{
		`waybar`,
		`"wlr/taskbar"`,
	} {
		if strings.Contains(bootstrapScript, notWant) {
			t.Fatalf("bootstrap script contains %q", notWant)
		}
	}

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Desktop = true
	cfg.DesktopEnv = "wayland"
	cfg.LocalContainer.WorkRoot = "/work/crabbox"
	got := localContainerReadyCheck(cfg)
	for _, want := range []string{
		"pgrep -x labwc",
		"pgrep -x wayvnc",
		"127.0.0.1:5900",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("wayland ready check missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "Xvfb :99") || strings.Contains(got, "x11vnc") {
		t.Fatalf("wayland ready check contains XFCE checks: %s", got)
	}

	cfg.DesktopEnv = "gnome"
	got = localContainerReadyCheck(cfg)
	for _, want := range []string{
		"pgrep -x labwc",
		"pgrep -x wayvnc",
		"127.0.0.1:5900",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("gnome ready check missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "pgrep -x gnome-shell") || strings.Contains(got, "Xvfb :99") {
		t.Fatalf("gnome ready check contains wrong compositor checks: %s", got)
	}
}

func TestBootstrapScriptSupportsDockerSocketCLI(t *testing.T) {
	for _, want := range []string{
		`[ "${CRABBOX_DOCKER_SOCKET:-0}" = "1" ] && ! command -v docker`,
		`https://download.docker.com/linux/${ID}/gpg`,
		`if apt-get update && apt-get install -y --no-install-recommends docker-ce-cli; then`,
		`rm -f /etc/apt/sources.list.d/docker.list`,
		`apt-get install -y --no-install-recommends docker-ce-cli`,
		`apt-get install -y --no-install-recommends docker.io`,
		`Docker-compatible socket requested but docker CLI is not installed`,
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

func TestTouchPreservesLocalContainerLabels(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	bootstrapDir := filepath.Join(os.TempDir(), "crabbox-bootstrap-touchtest")
	lease := core.LeaseTarget{
		LeaseID: "cbx_touch",
		Server: core.Server{
			Labels: map[string]string{
				"bootstrap_dir":  bootstrapDir,
				"docker_socket":  "1",
				"host_work_root": "/tmp/crabbox-local-container-work",
				"work_root":      "/tmp/crabbox-local-container-work",
				"ssh_user":       "runner",
			},
		},
	}
	server, err := b.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if server.Labels["work_root"] != lease.Server.Labels["work_root"] || server.Labels["host_work_root"] != lease.Server.Labels["host_work_root"] || server.Labels["docker_socket"] != "1" || server.Labels["ssh_user"] != "runner" {
		t.Fatalf("provider labels not preserved: %#v", server.Labels)
	}
	if server.Labels["bootstrap_dir"] != bootstrapDir {
		t.Fatalf("bootstrap_dir not preserved through touch: got %q want %q", server.Labels["bootstrap_dir"], bootstrapDir)
	}
	if server.Labels["state"] != "running" {
		t.Fatalf("state not touched: %#v", server.Labels)
	}
}

func TestHostLeaseWorkRootRequiresTrustedLabels(t *testing.T) {
	hostRoot := t.TempDir()
	leaseID := "cbx_trusted"
	if got := hostLeaseWorkRootFromLabels(leaseID, map[string]string{"docker_socket": "1", "work_root": hostRoot}); got != "" {
		t.Fatalf("unmarked work root accepted: %q", got)
	}
	if got := hostLeaseWorkRootFromLabels("Users", map[string]string{"docker_socket": "1", "work_root": "/"}); got != "" {
		t.Fatalf("spoofed lease root accepted: %q", got)
	}
	markTestLocalContainerWorkRoot(t, hostRoot)
	if got := hostLeaseWorkRootFromLabels(leaseID, map[string]string{"docker_socket": "1", "host_work_root": hostRoot, "work_root": "/work/crabbox"}); got != filepath.Join(hostRoot, leaseID) {
		t.Fatalf("work root=%q want %q", got, filepath.Join(hostRoot, leaseID))
	}
}

func TestTrustedBootstrapDir(t *testing.T) {
	tmpDir := os.TempDir()
	good := filepath.Join(tmpDir, "crabbox-bootstrap-abc123")
	if !trustedBootstrapDir(good) {
		t.Fatalf("should trust %q", good)
	}
	for _, bad := range []string{
		"",
		"crabbox-bootstrap-abc123",
		filepath.Join(tmpDir, "not-crabbox-dir"),
		filepath.Join(tmpDir, "crabbox-bootstrap-abc123", ".."),
		filepath.Join("/some/other/path", "crabbox-bootstrap-abc123"),
		"/etc/passwd",
	} {
		if trustedBootstrapDir(bad) {
			t.Fatalf("should reject %q", bad)
		}
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

func TestReleaseLeaseRemovesBootstrapDirAfterContainer(t *testing.T) {
	bootstrapDir, err := os.MkdirTemp("", "crabbox-bootstrap-release-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
	if err := os.WriteFile(filepath.Join(bootstrapDir, "bootstrap.sh"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	runner.run = func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if commandKey(req.Args) == commandKey([]string{"rm", "-f", "container123"}) {
			if _, err := os.Stat(bootstrapDir); err != nil {
				t.Fatalf("bootstrap directory removed before container teardown: %v", err)
			}
		}
		return core.LocalCommandResult{}, nil
	}
	b := testBackend(runner)
	lease := core.LeaseTarget{
		LeaseID: "cbx_release",
		Server: core.Server{
			CloudID: "container123",
			Labels:  map[string]string{"bootstrap_dir": bootstrapDir},
		},
	}

	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap directory still exists after release: %v", err)
	}
}

func TestReleaseLeaseDoesNotRemoveUntrustedBootstrapDir(t *testing.T) {
	parent := t.TempDir()
	bootstrapDir := filepath.Join(parent, "crabbox-bootstrap-forged")
	if err := os.MkdirAll(bootstrapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"rm", "-f", "container123"}): {},
		},
	}
	b := testBackend(runner)
	lease := core.LeaseTarget{
		LeaseID: "cbx_release",
		Server: core.Server{
			CloudID: "container123",
			Labels:  map[string]string{"bootstrap_dir": bootstrapDir},
		},
	}

	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bootstrapDir); err != nil {
		t.Fatalf("untrusted bootstrap directory removed: %v", err)
	}
}

func TestReleaseLeaseRemovesDockerSocketHostWorkRoot(t *testing.T) {
	hostRoot := t.TempDir()
	markTestLocalContainerWorkRoot(t, hostRoot)
	leaseRoot := filepath.Join(hostRoot, "cbx_release")
	if err := os.MkdirAll(filepath.Join(leaseRoot, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"rm", "-f", "container123"}): {},
		},
	}
	b := testBackend(runner)
	lease := core.LeaseTarget{
		LeaseID: "cbx_release",
		Server: core.Server{
			CloudID: "container123",
			Labels: map[string]string{
				"docker_socket": "1",
				"work_root":     hostRoot,
			},
		},
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leaseRoot); !os.IsNotExist(err) {
		t.Fatalf("host lease root still exists after release: %v", err)
	}
	if _, err := os.Stat(hostRoot); err != nil {
		t.Fatalf("host work root parent removed: %v", err)
	}
}

func TestReleaseLeaseWithIDResolvesHostWorkRoot(t *testing.T) {
	hostRoot := t.TempDir()
	markTestLocalContainerWorkRoot(t, hostRoot)
	leaseRoot := filepath.Join(hostRoot, "cbx_release")
	if err := os.MkdirAll(filepath.Join(leaseRoot, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	inspectJSON := `[{
		"Id":"container1234567890",
		"Name":"/crabbox-release",
		"Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_release","slug":"release-root","state":"ready","server_type":"ubuntu:24.04","ssh_user":"runner","work_root":"` + hostRoot + `","docker_socket":"1"}},
		"State":{"Status":"running","Running":true},
		"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}}
	}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "container1234567890\n"},
			commandKey([]string{"inspect", "container1234567890"}):  {Stdout: inspectJSON},
			commandKey([]string{"rm", "-f", "container1234567890"}): {},
		},
	}
	b := testBackend(runner)
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: "cbx_release"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leaseRoot); !os.IsNotExist(err) {
		t.Fatalf("host lease root still exists after release by id: %v", err)
	}
}

func TestCleanupRemovesExpiredLocalContainers(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	bootstrapDir, err := os.MkdirTemp("", "crabbox-bootstrap-cleanup-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bootstrapDir) })
	hostRoot := t.TempDir()
	markTestLocalContainerWorkRoot(t, hostRoot)
	leaseRoot := filepath.Join(hostRoot, "cbx_cleanup")
	if err := os.MkdirAll(filepath.Join(leaseRoot, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-48 * time.Hour).Unix()
	inspectJSON := `[{
		"Id":"abcdef1234567890",
		"Name":"/crabbox-cleanup",
		"Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_cleanup","slug":"old-cleanup","state":"ready","server_type":"ubuntu:24.04","ssh_user":"runner","work_root":"` + hostRoot + `","docker_socket":"1","bootstrap_dir":` + strconv.Quote(bootstrapDir) + `,"expires_at":"` + strconv.FormatInt(created, 10) + `"}},
		"State":{"Status":"running","Running":true},
		"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}}
	}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "abcdef1234567890\n"},
			commandKey([]string{"inspect", "abcdef1234567890"}):  {Stdout: inspectJSON},
			commandKey([]string{"rm", "-f", "abcdef1234567890"}): {},
		},
	}
	b := testBackend(runner)
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leaseRoot); !os.IsNotExist(err) {
		t.Fatalf("host lease root still exists after cleanup: %v", err)
	}
	if _, err := os.Stat(bootstrapDir); !os.IsNotExist(err) {
		t.Fatalf("bootstrap directory still exists after cleanup: %v", err)
	}
	args := recordedArgsForCommand(t, runner, "rm")
	if !strings.Contains(args, "abcdef1234567890") {
		t.Fatalf("cleanup did not remove container:\n%s", args)
	}
}

func TestCleanupRemovesClaimAfterHostWorkRootFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	claimDir := filepath.Join(stateHome, "crabbox", "claims")
	if err := os.MkdirAll(claimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	claimData := []byte(`{"leaseID":"cbx_cleanup","slug":"old-cleanup","provider":"` + providerName + `","repoRoot":` + strconv.Quote(t.TempDir()) + `,"claimedAt":` + strconv.Quote(expired) + `,"lastUsedAt":` + strconv.Quote(expired) + `,"idleTimeoutSeconds":60}`)
	if err := os.WriteFile(filepath.Join(claimDir, "cbx_cleanup.json"), claimData, 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath, err := core.TestboxKeyPath("cbx_cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostRoot := t.TempDir()
	markTestLocalContainerWorkRoot(t, hostRoot)
	leaseRoot := filepath.Join(hostRoot, "cbx_cleanup")
	if err := os.MkdirAll(filepath.Join(leaseRoot, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hostRoot, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(hostRoot, 0o700)
	})
	created := time.Now().Add(-48 * time.Hour).Unix()
	inspectJSON := `[{
		"Id":"abcdef1234567890",
		"Name":"/crabbox-cleanup",
		"Config":{"Image":"ubuntu:24.04","Labels":{"crabbox":"true","provider":"local-container","lease":"cbx_cleanup","slug":"old-cleanup","state":"ready","server_type":"ubuntu:24.04","ssh_user":"runner","work_root":"` + hostRoot + `","docker_socket":"1","expires_at":"` + strconv.FormatInt(created, 10) + `"}},
		"State":{"Status":"running","Running":true},
		"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"49153"}]}}
	}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ps", "-a", "--filter", "label=crabbox=true", "--filter", "label=provider=local-container", "--format", "{{.ID}}"}): {Stdout: "abcdef1234567890\n"},
			commandKey([]string{"inspect", "abcdef1234567890"}):  {Stdout: inspectJSON},
			commandKey([]string{"rm", "-f", "abcdef1234567890"}): {},
		},
	}
	b := testBackend(runner)
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err == nil {
		t.Fatal("cleanup succeeded despite host work root removal failure")
	}
	if claim, err := core.ReadLeaseClaim("cbx_cleanup"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("claim still exists after partial cleanup failure: %#v", claim)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stored key still exists after partial cleanup failure: %v", err)
	}
}

func TestCleanupRemovesClaimWithoutContainer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	scope := localContainerClaimScope("docker", "default")
	keyPath := writeLocalContainerClaimAndKey(t, "cbx_missing", "missing-container", scope)

	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"context", "show"}): {Stdout: "default\n"},
	}})
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_missing"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("claim still exists after cleanup: %#v", claim)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stored key still exists after cleanup: %v", err)
	}
}

func TestCleanupDryRunKeepsClaimWithoutContainer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	scope := localContainerClaimScope("docker", "default")
	keyPath := writeLocalContainerClaimAndKey(t, "cbx_missing", "missing-container", scope)

	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"context", "show"}): {Stdout: "default\n"},
	}})
	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_missing"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "cbx_missing" {
		t.Fatalf("claim removed during dry-run: %#v", claim)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key removed during dry-run: %v", err)
	}
}

func TestCleanupKeepsClaimFromDifferentRuntimeContext(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	keyPath := writeLocalContainerClaimAndKey(t, "cbx_other", "other-context", localContainerClaimScope("docker", "colima"))

	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"context", "show"}): {Stdout: "desktop-linux\n"},
	}})
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_other"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "cbx_other" {
		t.Fatalf("claim from another context was removed: %#v", claim)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key from another context was removed: %v", err)
	}
}

func TestCleanupKeepsClaimFromDifferentDockerHostSameContext(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	keyPath := writeLocalContainerClaimAndKey(t, "cbx_other_host", "other-host", localContainerClaimScope("docker", "default", "unix:///tmp/docker-a.sock"))
	t.Setenv("DOCKER_HOST", "unix:///tmp/docker-b.sock")

	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"context", "show"}): {Stdout: "default\n"},
	}})
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_other_host"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "cbx_other_host" {
		t.Fatalf("claim from another Docker host was removed: %#v", claim)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key from another Docker host was removed: %v", err)
	}
}

func TestCleanupRemovesStaleLegacyUnscopedClaimWithoutContainer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lastUsed := time.Now().Add(-48 * time.Hour).UTC()
	keyPath := writeLocalContainerClaimAndKeyAt(t, "cbx_legacy", "legacy-missing", "", lastUsed, time.Minute)

	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"context", "show"}): {Stdout: "default\n"},
	}})
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_legacy"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("stale legacy claim still exists after cleanup: %#v", claim)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("stale legacy stored key still exists after cleanup: %v", err)
	}
}

func writeLocalContainerClaimAndKey(t *testing.T, leaseID, slug string, scopes ...string) string {
	t.Helper()
	scope := ""
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	return writeLocalContainerClaimAndKeyAt(t, leaseID, slug, scope, time.Now().UTC(), time.Minute)
}

func writeLocalContainerClaimAndKeyAt(t *testing.T, leaseID, slug, scope string, lastUsed time.Time, idle time.Duration) string {
	t.Helper()
	if err := writeLocalContainerClaim(t, leaseID, slug, scope, lastUsed, idle); err != nil {
		t.Fatal(err)
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPath
}

func writeLocalContainerClaim(t *testing.T, leaseID, slug, scope string, lastUsed time.Time, idle time.Duration) error {
	t.Helper()
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		t.Fatal("XDG_STATE_HOME must be set before writing test claims")
	}
	claimDir := filepath.Join(stateHome, "crabbox", "claims")
	if err := os.MkdirAll(claimDir, 0o700); err != nil {
		return err
	}
	scopeField := ""
	if scope != "" {
		scopeField = `,"providerScope":` + strconv.Quote(scope)
	}
	data := []byte(`{"leaseID":` + strconv.Quote(leaseID) + `,"slug":` + strconv.Quote(slug) + `,"provider":` + strconv.Quote(providerName) + scopeField + `,"repoRoot":` + strconv.Quote(t.TempDir()) + `,"claimedAt":` + strconv.Quote(lastUsed.Format(time.RFC3339)) + `,"lastUsedAt":` + strconv.Quote(lastUsed.Format(time.RFC3339)) + `,"idleTimeoutSeconds":` + strconv.Itoa(int(idle.Seconds())) + `}`)
	return os.WriteFile(filepath.Join(claimDir, leaseID+".json"), data, 0o600)
}

func markTestLocalContainerWorkRoot(t *testing.T, root string) {
	t.Helper()
	if err := markLocalContainerWorkRoot(root); err != nil {
		t.Fatal(err)
	}
}
