package applecontainer

import (
	"context"
	"errors"
	"io"
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
	sequences map[string][]core.LocalCommandResult
	errors    map[string]error
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	key := commandKey(req.Args)
	if seq := r.sequences[key]; len(seq) > 0 {
		result := seq[0]
		r.sequences[key] = seq[1:]
		return result, nil
	}
	if err, ok := r.errors[key]; ok {
		return r.responses[key], err
	}
	if result, ok := r.responses[key]; ok {
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

func labelFromRunArgs(t *testing.T, args []string, key string) string {
	t.Helper()
	prefix := key + "="
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--label" && strings.HasPrefix(args[i+1], prefix) {
			return strings.TrimPrefix(args[i+1], prefix)
		}
	}
	t.Fatalf("%s label not found in run args: %v", key, args)
	return ""
}

func commandWasCalled(calls []core.LocalCommandRequest, command string) bool {
	for _, call := range calls {
		if len(call.Args) > 0 && call.Args[0] == command {
			return true
		}
	}
	return false
}

func testBackend(runner *recordingRunner) *backend {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleContainer = core.AppleContainerConfig{
		CLIPath:  "container",
		Image:    "debian:bookworm",
		User:     "runner",
		WorkRoot: "/work/crabbox",
		CPUs:     4,
		Memory:   "8g",
	}
	return newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
}

func sampleInspectJSON(id, slug, lease string) string {
	return `[{
		"status":"running",
		"configuration":{
			"id":"` + id + `",
			"image":{"reference":"debian:bookworm"},
			"hostname":"` + id + `",
			"labels":{"crabbox":"true","provider":"apple-container","lease":"` + lease + `","slug":"` + slug + `","state":"ready","ssh_user":"runner","work_root":"/work/crabbox","image":"debian:bookworm"}
		},
		"networks":[{"address":"192.168.64.7/24","gateway":"192.168.64.1","hostname":"` + id + `.test.","network":"default"}]
	}]`
}

func TestProviderSpecAndAliases(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name=%q want %s", p.Name(), providerName)
	}
	for _, alias := range []string{"apple-container", "apple", "applecontainer"} {
		got, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if got.Name() != providerName {
			t.Fatalf("ProviderFor(%q).Name=%q", alias, got.Name())
		}
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindSSHLease {
		t.Fatalf("kind=%v want ssh-lease", spec.Kind)
	}
	if spec.Family != "container" {
		t.Fatalf("family=%q want container", spec.Family)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want linux", spec.Targets)
	}
	if !spec.Features.Has(core.FeatureSSH) || !spec.Features.Has(core.FeatureCrabboxSync) || !spec.Features.Has(core.FeatureCleanup) || !spec.Features.Has(core.FeatureCacheVolume) {
		t.Fatalf("features=%#v want ssh, crabbox sync, cleanup, cache-volume", spec.Features)
	}
}

func TestAliasDoesNotCollideWithLocalContainer(t *testing.T) {
	// The bare "container" alias belongs to local-container; apple-container
	// must not steal it. Cross-provider registry collisions are asserted in
	// internal/providers/all; here we guard the provider's own alias set.
	for _, alias := range (Provider{}).Aliases() {
		if alias == "container" {
			t.Fatalf("apple-container must not declare the 'container' alias")
		}
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg := core.BaseConfig()
	wantImage := cfg.AppleContainer.Image
	cfg.Provider = providerName
	cfg.AppleContainer = core.AppleContainerConfig{}
	applyDefaults(&cfg)
	if cfg.AppleContainer.CLIPath != "container" || cfg.AppleContainer.Image != wantImage || cfg.AppleContainer.User != "crabbox" || cfg.AppleContainer.WorkRoot != "/work/crabbox" {
		t.Fatalf("defaults not applied: %#v", cfg.AppleContainer)
	}
	if cfg.SSHUser != "crabbox" || cfg.SSHPort != sshPort || cfg.WorkRoot != "/work/crabbox" {
		t.Fatalf("derived ssh fields wrong: user=%s port=%s work=%s", cfg.SSHUser, cfg.SSHPort, cfg.WorkRoot)
	}
}

func TestApplyDefaultsPreservesExplicitImage(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.AppleContainer.Image = "example-org/custom:tag"
	applyDefaults(&cfg)
	if cfg.AppleContainer.Image != "example-org/custom:tag" {
		t.Fatalf("explicit image was overwritten: %q", cfg.AppleContainer.Image)
	}
	if cfg.ServerType != "example-org/custom:tag" {
		t.Fatalf("server type=%q want explicit image", cfg.ServerType)
	}
}

func TestConfigForRunHonorsGlobalWorkRoot(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.WorkRoot = "/tmp/cbx"
	cfg.AppleContainer.WorkRoot = ""
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	got := b.configForRun()
	if got.WorkRoot != "/tmp/cbx" || got.AppleContainer.WorkRoot != "/tmp/cbx" {
		t.Fatalf("work root=%q apple=%q want /tmp/cbx", got.WorkRoot, got.AppleContainer.WorkRoot)
	}
}

func TestCreateContainerBuildsRunArgs(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.AppleContainer.ExtraRunArgs = []string{"--dns", "1.1.1.1"}
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "crabbox-blue\n"}

	id, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true)
	if err != nil {
		t.Fatal(err)
	}
	if id != "crabbox-blue" {
		t.Fatalf("id=%q", id)
	}
	call := runner.calls[0]
	if call.Name != "container" {
		t.Fatalf("binary=%q want container", call.Name)
	}
	args := strings.Join(call.Args, "\n")
	for _, want := range []string{
		"run\n-d",
		"--name\ncrabbox-blue",
		"--user\nroot",
		"-e\nCRABBOX_AUTHORIZED_KEY=ssh-ed25519 AAAA test",
		"-e\nCRABBOX_SSH_USER=runner",
		"-e\nCRABBOX_WORK_ROOT=/work/crabbox",
		"-e\nCRABBOX_SSH_PORT=22",
		"--label\nprovider=apple-container",
		"--label\nlease=cbx_123",
		"--label\nslug=blue-lobster",
		"--label\nssh_user=runner",
		"--cpus\n4",
		"--memory\n8g",
		"--dns\n1.1.1.1",
		"debian:bookworm",
		"/bin/sh",
		"-lc",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("run args missing %q:\n%s", want, args)
		}
	}
	// No host port publishing: Apple containers are reachable directly.
	if strings.Contains(args, "-p\n") {
		t.Fatalf("apple-container should not publish host ports:\n%s", args)
	}
	if strings.Count(args, "--dns") != 1 {
		t.Fatalf("custom DNS should not be duplicated:\n%s", args)
	}
}

func TestCreateContainerAddsHostDNS(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	runner.responses[commandKey([]string{"--dns"})] = core.LocalCommandResult{Stdout: "resolver #1\n  nameserver[0] : 10.0.0.2\n  nameserver[1] : 10.0.0.3\n"}
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "crabbox-blue\n"}
	if _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	if !strings.Contains(args, "--dns\n10.0.0.2") || !strings.Contains(args, "--dns\n10.0.0.3") {
		t.Fatalf("host DNS missing:\n%s", args)
	}
}

func TestAppleContainerHasDNSArg(t *testing.T) {
	for name, args := range map[string][]string{
		"split":    {"--dns", "1.1.1.1"},
		"equals":   {"--dns=1.1.1.1"},
		"dangling": {"--dns"},
		"disabled": {"--no-dns"},
	} {
		if !appleContainerHasDNSArg(args) {
			t.Fatalf("%s: expected DNS arg in %v", name, args)
		}
	}
	if appleContainerHasDNSArg([]string{"--hostname", "example"}) {
		t.Fatal("unexpected DNS arg")
	}
}

func TestParseAppleContainerDNSServers(t *testing.T) {
	text := `
resolver #1
  nameserver[0] : 10.0.0.2
  nameserver[1] : 127.0.0.1
  nameserver[2] : fe80::1%en0
nameserver 10.0.0.3
nameserver 2001:4860:4860::8888
nameserver garbage
`
	got := uniqueAppleContainerDNSServers(parseAppleContainerDNSServers(text), 3)
	want := []string{"10.0.0.2", "10.0.0.3", "2001:4860:4860::8888"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dns servers=%v want %v", got, want)
	}
}

func TestCreateContainerNoSecretsAsCLIArgs(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "crabbox-blue\n"}
	if _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue", "PUBKEY", true); err != nil {
		t.Fatal(err)
	}
	// The only key material passed is the public key (safe) via env. Ensure no
	// private-looking material leaks into args.
	args := recordedArgsForCommand(t, runner, "run")
	if strings.Contains(strings.ToUpper(args), "PRIVATE") {
		t.Fatalf("unexpected secret-like content in args:\n%s", args)
	}
}

func TestAcquirePostCreateFailureKeepsRetainedContainerKey(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container acquire only runs on macOS/Apple silicon")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ls", "--all", "--format", "json"}):       {Stdout: "[]"},
			commandKey([]string{"run"}):                                   {Stdout: "crabbox-kept\n"},
			commandKey([]string{"inspect", "crabbox-kept"}):               {Stderr: "inspect failed"},
			commandKey([]string{"delete", "--force", "crabbox-kept"}):     {},
			commandKey([]string{"system", "dns"}):                         {},
			commandKey([]string{"system", "dns", "--format", "resolver"}): {},
		},
		errors: map[string]error{
			commandKey([]string{"inspect", "crabbox-kept"}): errors.New("inspect failed"),
		},
	}
	b := testBackend(runner)

	if _, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true, Repo: core.Repo{Root: t.TempDir()}}); err == nil {
		t.Fatal("Acquire succeeded")
	}
	runArgs := recordedArgsForCommand(t, runner, "run")
	leaseID := labelFromRunArgs(t, strings.Split(runArgs, "\n"), "lease")
	if commandWasCalled(runner.calls, "delete") {
		t.Fatalf("kept failed container should not be deleted: %#v", runner.calls)
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("kept container SSH key missing: %v", err)
	}
	t.Cleanup(func() { core.RemoveStoredTestboxKey(leaseID) })
}

func TestCreateContainerMountsCacheVolumes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.Cache.Volumes = []core.CacheVolumeConfig{
		{Key: "my-app/linux node24 lock", Path: "/var/cache/crabbox/pnpm"},
		{Key: "npm-cache", Path: "/var/cache/crabbox/npm"},
	}
	runner.responses[commandKey([]string{"run"})] = core.LocalCommandResult{Stdout: "crabbox-blue\n"}

	if _, err := b.createContainer(context.Background(), cfg, "crabbox-blue", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test", true); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "run")
	root, err := appleContainerCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, volume := range cfg.Cache.Volumes {
		want := "--volume\n" + filepath.Join(root, appleContainerCacheVolumeName(volume.Key)) + ":" + volume.Path
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

func TestAppleContainerCacheVolumeNameIsStableAndFilesystemSafe(t *testing.T) {
	got := appleContainerCacheVolumeName("My App/linux node24 lock")
	again := appleContainerCacheVolumeName("My App/linux node24 lock")
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

func TestListContainersFiltersByLabel(t *testing.T) {
	lsJSON := `[
		` + strings.TrimPrefix(strings.TrimSuffix(sampleInspectJSON("crabbox-blue", "blue-lobster", "cbx_123"), "]"), "[") + `,
		{"status":"running","configuration":{"id":"someone-elses","image":{"reference":"alpine"},"labels":{"app":"web"}},"networks":[{"address":"192.168.64.9/24"}]}
	]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ls", "--all", "--format", "json"}): {Stdout: lsJSON},
		},
	}
	b := testBackend(runner)
	containers, err := b.listContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 {
		t.Fatalf("containers=%d want 1 (crabbox-owned only)", len(containers))
	}
	v := b.serverFromContainer(containers[0], b.configForRun())
	if v.Provider != providerName || v.CloudID != "crabbox-blue" || v.Labels["ssh_port"] != "22" || v.PublicNet.IPv4.IP != "192.168.64.7" {
		t.Fatalf("unexpected view: %#v", v)
	}
}

func TestListContainersParsesObjectStatusNetworks(t *testing.T) {
	lsJSON := `[{
		"status":{
			"state":"running",
			"networks":[{"address":"192.168.64.21/24","gateway":"192.168.64.1"}]
		},
		"configuration":{
			"id":"crabbox-object-status",
			"image":{"reference":"debian:bookworm"},
			"labels":{"crabbox":"true","provider":"apple-container","lease":"cbx_456","slug":"object-status","state":"ready","ssh_user":"runner","work_root":"/work/crabbox","image":"debian:bookworm"}
		}
	}]`
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ls", "--all", "--format", "json"}): {Stdout: lsJSON},
		},
	}
	b := testBackend(runner)
	containers, err := b.listContainers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 {
		t.Fatalf("containers=%d want 1", len(containers))
	}
	v := b.serverFromContainer(containers[0], b.configForRun())
	if v.CloudID != "crabbox-object-status" || v.Status != "ready" || v.PublicNet.IPv4.IP != "192.168.64.21" {
		t.Fatalf("unexpected view: %#v", v)
	}
}

func TestResolveAndSSHTarget(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"ls", "--all", "--format", "json"}): {Stdout: sampleInspectJSON("crabbox-blue", "blue-lobster", "cbx_123")},
		},
	}
	b := testBackend(runner)
	c, leaseID, slug, err := b.resolveContainer(context.Background(), "blue-lobster")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := b.prepareLease(context.Background(), b.configForRun(), c, leaseID, slug, false)
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_123" {
		t.Fatalf("lease id=%q", lease.LeaseID)
	}
	if lease.SSH.Host != "192.168.64.7" || lease.SSH.Port != "22" || lease.SSH.User != "runner" || lease.SSH.TargetOS != core.TargetLinux {
		t.Fatalf("unexpected ssh target: %#v", lease.SSH)
	}
	if !strings.Contains(lease.SSH.ReadyCheck, "rsync --version") || !strings.Contains(lease.SSH.ReadyCheck, "test -d '/work/crabbox'") {
		t.Fatalf("ready check missing expectations: %q", lease.SSH.ReadyCheck)
	}
}

func TestRuntimeMethodsRejectUnsupportedHostBeforeContainerCLI(t *testing.T) {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		t.Skip("unsupported-host gate only applies off macOS/Apple silicon")
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{}}
	b := testBackend(runner)
	if _, err := b.List(context.Background(), core.ListRequest{}); err == nil || !strings.Contains(err.Error(), "Apple silicon") {
		t.Fatalf("List error=%v, want Apple silicon gate", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("unsupported host should not invoke container CLI: %#v", runner.calls)
	}
}

func TestBootstrapScriptToleratesCacheVolumeChownFailures(t *testing.T) {
	for _, want := range []string{
		`CRABBOX_CACHE_VOLUME_PATH_`,
		`chown -R "$user" "$cache_path" 2>/dev/null || true`,
	} {
		if !strings.Contains(bootstrapScript, want) {
			t.Fatalf("bootstrap script missing %q", want)
		}
	}
}

func TestPrepareLeaseRequiresNetworkAddress(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	cfg := b.configForRun()
	c := inspectContainer{
		Status:        inspectStatus{State: "running"},
		Configuration: inspectConfiguration{ID: "crabbox-noip", Labels: map[string]string{"crabbox": "true", "provider": providerName, "ssh_user": "runner", "work_root": "/work/crabbox"}},
	}
	if _, err := b.prepareLease(context.Background(), cfg, c, "cbx_x", "x", false); err == nil {
		t.Fatal("prepareLease accepted a container without a network address")
	}
}

func TestWaitForNetworkAddressPollsInspect(t *testing.T) {
	runner := &recordingRunner{
		sequences: map[string][]core.LocalCommandResult{
			commandKey([]string{"inspect", "crabbox-wait"}): {
				{Stdout: `[{"status":"running","configuration":{"id":"crabbox-wait","labels":{"crabbox":"true","provider":"apple-container"}}}]`},
				{Stdout: sampleInspectJSON("crabbox-wait", "wait", "cbx_wait")},
			},
		},
	}
	b := testBackend(runner)
	start := inspectContainer{
		Status:        inspectStatus{State: "running"},
		Configuration: inspectConfiguration{ID: "crabbox-wait", Labels: map[string]string{"crabbox": "true", "provider": providerName}},
	}
	c, err := b.waitForNetworkAddress(context.Background(), "crabbox-wait", start, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ip(); got != "192.168.64.7" {
		t.Fatalf("ip=%q want 192.168.64.7", got)
	}
	args := recordedArgsForCommand(t, runner, "inspect")
	if !strings.Contains(args, "inspect\ncrabbox-wait") {
		t.Fatalf("inspect args=%q", args)
	}
}

func TestWaitForNetworkAddressStopsOnExitedContainer(t *testing.T) {
	b := testBackend(&recordingRunner{responses: map[string]core.LocalCommandResult{}})
	start := inspectContainer{
		Status:        inspectStatus{State: "stopped"},
		Configuration: inspectConfiguration{ID: "crabbox-exited", Labels: map[string]string{"crabbox": "true", "provider": providerName}},
	}
	if _, err := b.waitForNetworkAddress(context.Background(), "crabbox-exited", start, time.Minute); err == nil || !strings.Contains(err.Error(), "stopped before a network address") {
		t.Fatalf("error=%v, want stopped before network address", err)
	}
}

func TestExitedDuringBootstrapErrorIncludesLogsAndDNSHint(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"logs", "crabbox-exited"}): {Stdout: "Err: Temporary failure resolving 'archive.ubuntu.com'\n"},
		},
	}
	b := testBackend(runner)
	err := b.exitedDuringBootstrapError(context.Background(), "crabbox-exited", "stopped")
	if err == nil {
		t.Fatal("expected bootstrap error")
	}
	msg := err.Error()
	for _, want := range []string{"stopped during SSH bootstrap", "Temporary failure resolving", "--apple-container-extra-run-args '--dns <resolver>'"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error missing %q:\n%s", want, msg)
		}
	}
}

func TestRemoveContainerUsesDeleteForce(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"delete", "--force", "crabbox-blue"}): {},
		},
	}
	b := testBackend(runner)
	if err := b.removeContainer(context.Background(), "crabbox-blue"); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "delete")
	if !strings.Contains(args, "delete\n--force\ncrabbox-blue") {
		t.Fatalf("delete args=%q", args)
	}
}

func TestCleanupPreservesUnclaimedStoppedAppleContainer(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container cleanup only runs on macOS/Apple silicon")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ls", "--all", "--format", "json"}): {Stdout: `[{
			"status":"stopped",
			"configuration":{
				"id":"unclaimed-container",
				"image":{"reference":"debian:bookworm"},
				"labels":{"crabbox":"true","provider":"apple-container"}
			}
		}]`},
	}}

	if err := testBackend(runner).Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if commandWasCalled(runner.calls, "delete") {
		t.Fatalf("Cleanup deleted unclaimed stopped container: %#v", runner.calls)
	}
}

func TestCleanupDeletesStoppedAppleContainerWithExactClaim(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container cleanup only runs on macOS/Apple silicon")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const leaseID = "cbx_claimed_cleanup"
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID,
		"claimed-cleanup",
		providerName,
		"",
		"",
		t.TempDir(),
		time.Minute,
		false,
		core.Server{CloudID: "claimed-container", Provider: providerName},
		core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ls", "--all", "--format", "json"}): {Stdout: `[{
			"status":"stopped",
			"configuration":{
				"id":"claimed-container",
				"image":{"reference":"debian:bookworm"},
				"labels":{"crabbox":"true","provider":"apple-container","lease":"cbx_claimed_cleanup"}
			}
		}]`},
	}}

	if err := testBackend(runner).Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if !commandWasCalled(runner.calls, "delete") {
		t.Fatalf("Cleanup preserved exactly claimed stopped container: %#v", runner.calls)
	}
}

func TestShouldCleanupRequiresExactClaimBeforeStateDeletion(t *testing.T) {
	now := time.Now().UTC()
	server := core.Server{
		CloudID: "claimed-container",
		Status:  "stopped",
		Labels: map[string]string{
			"crabbox":  "true",
			"provider": providerName,
			"lease":    "cbx_claimed_container",
		},
	}
	exactClaim := core.LeaseClaim{
		LeaseID:  "cbx_claimed_container",
		Provider: providerName,
		CloudID:  "claimed-container",
	}

	if cleanup, reason := shouldCleanup(server, core.LeaseClaim{}, false, now); cleanup || reason != "missing claim" {
		t.Fatalf("unclaimed cleanup=%v reason=%q, want false/missing claim", cleanup, reason)
	}
	conflictingClaim := exactClaim
	conflictingClaim.CloudID = "other-container"
	if cleanup, reason := shouldCleanup(server, conflictingClaim, true, now); cleanup || reason != "claim mismatch" {
		t.Fatalf("conflicting cleanup=%v reason=%q, want false/claim mismatch", cleanup, reason)
	}
	if cleanup, reason := shouldCleanup(server, exactClaim, true, now); !cleanup || reason != "container state=stopped" {
		t.Fatalf("exact cleanup=%v reason=%q, want true/container state=stopped", cleanup, reason)
	}
}

func TestRequireExactAppleContainerClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_claimed_apple_container"
	if err := requireExactAppleContainerClaim(leaseID, "owned-container"); err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("unclaimed error=%v, want exact-claim rejection", err)
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID,
		"claimed-apple-container",
		providerName,
		"",
		"",
		t.TempDir(),
		time.Minute,
		false,
		core.Server{CloudID: "owned-container", Provider: providerName, Labels: map[string]string{"provider": providerName, "slug": "claimed-apple-container"}},
		core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	if err := requireExactAppleContainerClaim(leaseID, "other-container"); err == nil || !strings.Contains(err.Error(), "bound to container") {
		t.Fatalf("mismatched container error=%v, want resource-binding rejection", err)
	}
	if err := requireExactAppleContainerClaim(leaseID, "owned-container"); err != nil {
		t.Fatalf("claimed lease rejected: %v", err)
	}
}

func TestResolveRawAppleContainerRequiresExplicitReclaimAndPersistsBinding(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container resolve only runs on macOS/Apple silicon")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ls", "--all", "--format", "json"}):          {Stdout: sampleInspectJSON("raw-apple-container", "raw-apple", "cbx_raw_apple")},
		commandKey([]string{"delete", "--force", "raw-apple-container"}): {},
	}}
	b := testBackend(runner)
	repo := core.Repo{Root: t.TempDir()}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "raw-apple-container", Repo: repo}); err == nil || !strings.Contains(err.Error(), "explicit --reclaim") {
		t.Fatalf("Resolve without reclaim error=%v", err)
	}
	if claim, err := core.ReadLeaseClaim("cbx_raw_apple"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("non-reclaim resolve minted claim: %#v", claim)
	}
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "raw-apple-container", Repo: repo, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim("cbx_raw_apple")
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "raw-apple-container" {
		t.Fatalf("reclaim binding=%#v", claim)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if !commandWasCalled(runner.calls, "delete") {
		t.Fatalf("reclaimed container was not released: %#v", runner.calls)
	}
}

func TestLegacyAppleContainerClaimRequiresReclaimBeforeStop(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container resolve only runs on macOS/Apple silicon")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_legacy_apple"
	if err := core.ClaimLeaseForRepoProviderScopePond(leaseID, "legacy-apple", providerName, "", "", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ls", "--all", "--format", "json"}):             {Stdout: sampleInspectJSON("legacy-apple-container", "legacy-apple", leaseID)},
		commandKey([]string{"delete", "--force", "legacy-apple-container"}): {},
	}}
	b := testBackend(runner)
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: core.Server{CloudID: "legacy-apple-container"}}}); err == nil || !strings.Contains(err.Error(), "explicit --reclaim") {
		t.Fatalf("legacy direct stop error=%v", err)
	}
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "legacy-apple-container" {
		t.Fatalf("legacy reclaim binding=%#v", claim)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
}

func TestResolveReclaimDoesNotRetargetBoundAppleContainerClaim(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container resolve only runs on macOS/Apple silicon")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_bound_apple"
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID,
		"bound-apple",
		providerName,
		"",
		"",
		t.TempDir(),
		time.Minute,
		false,
		core.Server{CloudID: "container-a", Provider: providerName, Labels: map[string]string{"provider": providerName, "slug": "bound-apple"}},
		core.SSHTarget{},
	); err != nil {
		t.Fatal(err)
	}
	listJSON := `[
		{"status":"running","configuration":{"id":"container-b","image":{"reference":"debian:bookworm"},"labels":{"crabbox":"true","provider":"apple-container","lease":"cbx_bound_apple","slug":"bound-apple","state":"ready","ssh_user":"runner","work_root":"/work/crabbox"}},"networks":[{"address":"192.168.64.8/24"}]},
		{"status":"running","configuration":{"id":"container-a","image":{"reference":"debian:bookworm"},"labels":{"crabbox":"true","provider":"apple-container","lease":"cbx_stale_label","slug":"stale-label","state":"ready","ssh_user":"runner","work_root":"/work/crabbox"}},"networks":[{"address":"192.168.64.7/24"}]}
	]`
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ls", "--all", "--format", "json"}): {Stdout: listJSON},
	}}
	b := testBackend(runner)
	repo := core.Repo{Root: t.TempDir()}
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: leaseID, Repo: repo, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "container-a" || lease.LeaseID != leaseID {
		t.Fatalf("exact claim resolved container=%q lease=%q, want container-a/%s", lease.Server.CloudID, lease.LeaseID, leaseID)
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "container-b", Repo: repo, Reclaim: true}); err == nil || !strings.Contains(err.Error(), "bound to container") {
		t.Fatalf("raw conflicting reclaim error=%v", err)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.CloudID != "container-a" {
		t.Fatalf("bound claim was retargeted: %#v", claim)
	}
}

func TestResolveStatusOnlyAllowsClaimlessAppleContainerWithoutClaiming(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container resolve only runs on macOS/Apple silicon")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ls", "--all", "--format", "json"}): {Stdout: sampleInspectJSON("status-apple", "status-apple", "cbx_status_apple")},
	}}
	b := testBackend(runner)
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "status-apple", Repo: core.Repo{Root: t.TempDir()}, StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "status-apple" {
		t.Fatalf("status lease=%#v", lease)
	}
	if claim, err := core.ReadLeaseClaim("cbx_status_apple"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("status-only resolve minted claim: %#v", claim)
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		"cbx_status_apple",
		"status-apple",
		providerName,
		"",
		"",
		t.TempDir(),
		time.Minute,
		false,
		lease.Server,
		lease.SSH,
	); err != nil {
		t.Fatal(err)
	}
	before, err := core.ReadLeaseClaim("cbx_status_apple")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "status-apple", Repo: core.Repo{Root: t.TempDir()}, StatusOnly: true}); err != nil {
		t.Fatalf("owned status-only resolve: %v", err)
	}
	after, err := core.ReadLeaseClaim("cbx_status_apple")
	if err != nil {
		t.Fatal(err)
	}
	if after.RepoRoot != before.RepoRoot || after.LastUsedAt != before.LastUsedAt || after.CloudID != before.CloudID {
		t.Fatalf("status-only resolve mutated claim: before=%#v after=%#v", before, after)
	}
}

func TestResolveReclaimRejectsMetadataLessAppleContainer(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container resolve only runs on macOS/Apple silicon")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"ls", "--all", "--format", "json"}): {Stdout: sampleInspectJSON("metadata-less", "", "")},
	}}
	b := testBackend(runner)
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "metadata-less", Repo: core.Repo{Root: t.TempDir()}, Reclaim: true}); err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("metadata-less reclaim error=%v", err)
	}
}

func TestReleaseLeaseRejectsUnclaimedContainer(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("apple-container release only runs on macOS/Apple silicon")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"delete", "--force", "unclaimed-container"}): {},
	}}
	b := testBackend(runner)
	err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{
		LeaseID: "cbx_unclaimed_apple_container",
		Server:  core.Server{CloudID: "unclaimed-container"},
	}})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("ReleaseLease error=%v, want exact-claim rejection", err)
	}
	if commandWasCalled(runner.calls, "delete") {
		t.Fatalf("unclaimed container reached delete: %#v", runner.calls)
	}
}

func TestDoctorReportsMissingCLI(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("doctor CLI probe only exercised on macOS/Apple silicon")
	}
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"system", "status"}): {Stderr: "command not found"},
		},
		errors: map[string]error{
			commandKey([]string{"system", "status"}): errFake("exit status 127"),
		},
	}
	b := testBackend(runner)
	if _, err := b.Doctor(context.Background(), core.DoctorRequest{}); err == nil {
		t.Fatal("doctor passed despite failing system status")
	}
}

func TestDoctorReadyWhenSystemRunning(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("doctor only reports ready on macOS/Apple silicon")
	}
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"system", "status"}):                {Stdout: "running\n"},
			commandKey([]string{"run", "--help"}):                   {Stdout: "OPTIONS:\n  -u, --user <user>\n  --label <label>\n  --dns <server>\n"},
			commandKey([]string{"ls", "--all", "--format", "json"}): {Stdout: "[]"},
		},
	}
	b := testBackend(runner)
	res, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != providerName || !strings.Contains(res.Message, "system=ready") || !strings.Contains(res.Message, "run=ready") {
		t.Fatalf("doctor result=%#v", res)
	}
}

func TestDoctorRejectsIncompatibleRunCLI(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("doctor run-surface probe only exercised on macOS/Apple silicon")
	}
	// `system status` succeeds but the `run` subcommand is missing/incompatible.
	t.Run("run subcommand missing", func(t *testing.T) {
		runner := &recordingRunner{
			responses: map[string]core.LocalCommandResult{
				commandKey([]string{"system", "status"}): {Stdout: "running\n"},
			},
			errors: map[string]error{
				commandKey([]string{"run", "--help"}): errFake("unknown subcommand \"run\""),
			},
		}
		if _, err := testBackend(runner).Doctor(context.Background(), core.DoctorRequest{}); err == nil {
			t.Fatal("doctor passed despite missing run subcommand")
		}
	})
	// `run --help` works but does not advertise the options the lease path needs.
	t.Run("run missing required options", func(t *testing.T) {
		runner := &recordingRunner{
			responses: map[string]core.LocalCommandResult{
				commandKey([]string{"system", "status"}): {Stdout: "running\n"},
				commandKey([]string{"run", "--help"}):    {Stdout: "OPTIONS:\n  --memory <size>\n"},
			},
		}
		if _, err := testBackend(runner).Doctor(context.Background(), core.DoctorRequest{}); err == nil {
			t.Fatal("doctor passed despite run lacking --user/--label/--dns")
		}
	})
	t.Run("run missing dns option", func(t *testing.T) {
		runner := &recordingRunner{
			responses: map[string]core.LocalCommandResult{
				commandKey([]string{"system", "status"}): {Stdout: "running\n"},
				commandKey([]string{"run", "--help"}):    {Stdout: "OPTIONS:\n  -u, --user <user>\n  --label <label>\n"},
			},
		}
		if _, err := testBackend(runner).Doctor(context.Background(), core.DoctorRequest{}); err == nil {
			t.Fatal("doctor passed despite run lacking --dns")
		}
	})
}

func TestRequireMacOSGate(t *testing.T) {
	err := requireMacOS()
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		if err != nil {
			t.Fatalf("requireMacOS errored on darwin/arm64: %v", err)
		}
		return
	}
	// Every other host, including darwin/amd64, must be rejected.
	if err == nil {
		t.Fatal("requireMacOS should reject hosts that are not darwin/arm64")
	}
	if !strings.Contains(err.Error(), "Apple silicon") {
		t.Fatalf("unexpected gate error: %v", err)
	}
}

func TestConfigureRejectsTailscaleAndNonLinux(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Tailscale.Enabled = true
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted tailscale")
	}
	cfg = core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetWindows
	if _, err := (Provider{}).Configure(cfg, core.Runtime{}); err == nil {
		t.Fatal("Configure accepted non-linux target")
	}
}

func TestInspectIPStripsCIDR(t *testing.T) {
	for name, tc := range map[string]struct {
		container inspectContainer
		want      string
	}{
		"top-level address": {
			container: inspectContainer{Networks: []inspectNetwork{{Address: "192.168.64.42/24"}}},
			want:      "192.168.64.42",
		},
		"bare address": {
			container: inspectContainer{Networks: []inspectNetwork{{Address: "10.0.0.5"}}},
			want:      "10.0.0.5",
		},
		"ipv4 address": {
			container: inspectContainer{Networks: []inspectNetwork{{IPv4Address: "192.168.64.4/24"}}},
			want:      "192.168.64.4",
		},
		"status networks": {
			container: inspectContainer{Status: inspectStatus{Networks: []inspectNetwork{{Address: "192.168.64.55/24"}}}},
			want:      "192.168.64.55",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.container.ip(); got != tc.want {
				t.Fatalf("ip=%q want %s", got, tc.want)
			}
		})
	}
}

func TestDecodeInspectToleratesStringImage(t *testing.T) {
	containers, err := decodeInspect([]byte(`[{"status":"running","configuration":{"id":"x","image":"alpine:3"},"networks":[]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || containers[0].image() != "alpine:3" {
		t.Fatalf("decoded=%#v", containers)
	}
}

func TestDecodeInspectToleratesObjectStatus(t *testing.T) {
	containers, err := decodeInspect([]byte(`[{
		"status":{
			"state":"running",
			"networks":[{"address":"192.168.64.12/24","gateway":"192.168.64.1"}]
		},
		"configuration":{
			"id":"crabbox-one",
			"image":{"reference":"debian:bookworm"},
			"labels":{"crabbox":"true","provider":"apple-container"}
		}
	}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 {
		t.Fatalf("containers=%d want 1", len(containers))
	}
	c := containers[0]
	if got := c.status(); got != "running" {
		t.Fatalf("status=%q want running", got)
	}
	if got := c.ip(); got != "192.168.64.12" {
		t.Fatalf("ip=%q want 192.168.64.12", got)
	}
	if got := c.image(); got != "debian:bookworm" {
		t.Fatalf("image=%q want debian:bookworm", got)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
