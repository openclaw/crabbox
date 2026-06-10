package multipass

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type recordingRunner struct {
	calls     []core.LocalCommandRequest
	cloudInit string
	responses map[string]core.LocalCommandResult
	errors    map[string]error
}

func (r *recordingRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if len(req.Args) > 0 && req.Args[0] == "launch" {
		r.cloudInit = readLaunchCloudInit(req.Args)
	}
	key := commandKey(req.Args)
	if err, ok := r.errors[key]; ok {
		return r.responses[key], err
	}
	if result, ok := r.responses[key]; ok {
		return result, nil
	}
	if len(req.Args) >= 4 && req.Args[0] == "info" {
		if err, ok := r.errors["info"]; ok {
			return r.responses["info"], err
		}
		if result, ok := r.responses["info"]; ok {
			result.Stdout = strings.ReplaceAll(result.Stdout, "{{name}}", req.Args[3])
			return result, nil
		}
	}
	if len(req.Args) > 0 {
		if err, ok := r.errors[req.Args[0]]; ok {
			return r.responses[req.Args[0]], err
		}
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
	cfg.Multipass = core.MultipassConfig{
		CLIPath:       "multipass",
		Image:         "24.04",
		User:          "runner",
		WorkRoot:      "/workspace/crabbox",
		CPUs:          4,
		Memory:        "8G",
		Disk:          "40G",
		LaunchTimeout: 10 * time.Minute,
	}
	return newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: io.Discard, Stderr: io.Discard, Exec: runner}).(*backend)
}

func sampleListJSON() string {
	return `{"list":[{"name":"crabbox-blue-1234abcd","state":"Running","ipv4":["192.168.64.7"],"release":"Ubuntu 24.04 LTS"},{"name":"primary","state":"Stopped","ipv4":[],"release":"Ubuntu 24.04 LTS"}]}`
}

func sampleInfoJSON(name string) string {
	return `{"errors":[],"info":{"` + name + `":{"state":"Running","ipv4":["192.168.64.7"],"release":"Ubuntu 24.04.4 LTS","image_hash":"abc123","image_release":"24.04 LTS"}}}`
}

func TestProviderSpecAndAliases(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name=%q want %s", p.Name(), providerName)
	}
	for _, alias := range []string{"multipass", "mp", "canonical-multipass"} {
		got, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if got.Name() != providerName {
			t.Fatalf("ProviderFor(%q).Name=%q", alias, got.Name())
		}
	}
	spec := p.Spec()
	if spec.Kind != core.ProviderKindSSHLease || spec.Family != "local-vm" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureCleanup, core.FeatureCacheVolume} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Multipass = core.MultipassConfig{}
	applyDefaults(&cfg)
	if cfg.Multipass.CLIPath != "multipass" || cfg.Multipass.Image != "26.04" || cfg.Multipass.User != "crabbox" || cfg.Multipass.WorkRoot != "/work/crabbox" {
		t.Fatalf("defaults not applied: %#v", cfg.Multipass)
	}
	if cfg.SSHUser != "crabbox" || cfg.SSHPort != sshPort || len(cfg.SSHFallbackPorts) != 0 || cfg.WorkRoot != "/work/crabbox" {
		t.Fatalf("derived SSH fields wrong: user=%s port=%s fallback=%v work=%s", cfg.SSHUser, cfg.SSHPort, cfg.SSHFallbackPorts, cfg.WorkRoot)
	}
}

func TestCreateInstanceBuildsLaunchArgsAndCloudInit(t *testing.T) {
	oldHostOS := multipassHostOS
	multipassHostOS = "darwin"
	t.Cleanup(func() { multipassHostOS = oldHostOS })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"get", "local.driver"}): {Stdout: "qemu\n"},
		"launch": {},
	}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.Cache.Volumes = []core.CacheVolumeConfig{
		{Key: "my-app/linux node24 lock", Path: "/var/cache/crabbox/pnpm"},
	}

	if err := b.createInstance(context.Background(), cfg, "crabbox-blue-1234abcd", "cbx_123", "blue-lobster", "ssh-ed25519 AAAA test"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) == 0 {
		t.Fatal("no commands recorded")
	}
	launchArgs := recordedArgsForCommand(t, runner, "launch")
	call := runner.calls[0]
	if call.Name != "multipass" {
		t.Fatalf("binary=%q want multipass", call.Name)
	}
	args := launchArgs
	root, err := multipassCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"launch\n--name\ncrabbox-blue-1234abcd",
		"--cpus\n4",
		"--memory\n8G",
		"--disk\n40G",
		"--timeout\n600",
		"24.04",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("launch args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "--mount") {
		t.Fatalf("darwin qemu launch should not include --mount:\n%s", args)
	}
	mountArgs := recordedArgsForCommand(t, runner, "mount")
	for _, want := range []string{"mount\n--type\nnative", filepath.Join(root, multipassCacheVolumeName("my-app/linux node24 lock")), "crabbox-blue-1234abcd:/var/cache/crabbox/pnpm"} {
		if !strings.Contains(mountArgs, want) {
			t.Fatalf("native mount args missing %q:\n%s", want, mountArgs)
		}
	}
	cloudInit := runner.cloudInit
	for _, want := range []string{
		`name: "runner"`,
		"ssh-ed25519 AAAA test",
		"test -w '/workspace/crabbox'",
		"Port 22",
	} {
		if !strings.Contains(cloudInit, want) {
			t.Fatalf("cloud-init missing %q:\n%s", want, cloudInit)
		}
	}
}

func TestCreateInstanceFallsBackToClassicMountsForDarwinVirtualBox(t *testing.T) {
	oldHostOS := multipassHostOS
	multipassHostOS = "darwin"
	t.Cleanup(func() { multipassHostOS = oldHostOS })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"get", "local.driver"}): {Stdout: "virtualbox\n"},
		"launch": {},
	}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.Cache.Volumes = []core.CacheVolumeConfig{{Key: "gomod", Path: "/var/cache/crabbox/go"}}

	if err := b.createInstance(context.Background(), cfg, "crabbox-blue-1234abcd", "cbx_123", "blue-lobster", "PUB"); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "launch")
	root, err := multipassCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	mountArg := filepath.Join(root, multipassCacheVolumeName("gomod")) + ":/var/cache/crabbox/go"
	if !strings.Contains(args, "--mount\n"+mountArg) {
		t.Fatalf("virtualbox launch args missing classic mount %q:\n%s", mountArg, args)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "mount" {
			t.Fatalf("virtualbox should not use native mount command: %#v", call.Args)
		}
	}
}

func readLaunchCloudInit(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--cloud-init" {
			data, err := os.ReadFile(args[i+1])
			if err != nil {
				return ""
			}
			return string(data)
		}
	}
	return ""
}

func TestListAndResolveInstancesWithClaim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	claimServer := core.Server{
		CloudID: "crabbox-blue-1234abcd",
		Labels: map[string]string{
			"crabbox":   "true",
			"provider":  providerName,
			"lease":     "cbx_123",
			"slug":      "blue-lobster",
			"instance":  "crabbox-blue-1234abcd",
			"ssh_user":  "runner",
			"ssh_port":  "22",
			"work_root": "/workspace/crabbox",
		},
	}
	claimTarget := core.SSHTarget{Host: "192.168.64.7", Port: "22"}
	if err := claimLeaseForRepoProviderScopePond("cbx_123", "blue-lobster", providerName, instanceScope("crabbox-blue-1234abcd"), "", t.TempDir(), 30*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimEndpoint("cbx_123", claimServer, claimTarget); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"list", "--format", "json"}):                          {Stdout: sampleListJSON()},
		commandKey([]string{"info", "--format", "json", "crabbox-blue-1234abcd"}): {Stdout: sampleInfoJSON("crabbox-blue-1234abcd")},
	}}
	b := testBackend(runner)

	views, err := b.List(context.Background(), core.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views=%d want 1", len(views))
	}
	if views[0].Provider != providerName || views[0].CloudID != "crabbox-blue-1234abcd" || views[0].Labels["slug"] != "blue-lobster" || views[0].PublicNet.IPv4.IP != "192.168.64.7" {
		t.Fatalf("unexpected view: %#v", views[0])
	}
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "blue-lobster"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_123" || lease.SSH.Host != "192.168.64.7" || lease.SSH.Port != "22" || lease.SSH.User != "runner" || len(lease.SSH.FallbackPorts) != 0 || lease.SSH.ReadyCheck != "/usr/local/bin/crabbox-ready" {
		t.Fatalf("unexpected lease: %#v", lease)
	}
}

func TestDoctorReady(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"version"}):                  {Stdout: "multipass 1.16.0\nmultipassd 1.16.0\n"},
		commandKey([]string{"list", "--format", "json"}): {Stdout: `{"list":[]}`},
	}}
	b := testBackend(runner)
	res, err := b.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != providerName || !strings.Contains(res.Message, "daemon=ready") || !strings.Contains(res.Message, "multipass 1.16.0") {
		t.Fatalf("doctor result=%#v", res)
	}
}

func TestRemoveInstanceUsesDeletePurge(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"delete", "--purge", "crabbox-blue-1234abcd"}): {},
	}}
	b := testBackend(runner)
	if err := b.removeInstance(context.Background(), "crabbox-blue-1234abcd"); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "delete")
	if !strings.Contains(args, "delete\n--purge\ncrabbox-blue-1234abcd") {
		t.Fatalf("delete args=%q", args)
	}
}

func TestAcquireCleansUpLaunchFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--format", "json"}): {Stdout: `{"list":[]}`},
			"launch": {Stderr: "launch failed"},
		},
		errors: map[string]error{"launch": errors.New("launch failed")},
	}
	b := testBackend(runner)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true})
	if err == nil || !strings.Contains(err.Error(), "multipass launch failed") {
		t.Fatalf("Acquire error=%v", err)
	}
	deleteArgs := recordedArgsForCommand(t, runner, "delete")
	if !strings.Contains(deleteArgs, "delete\n--purge\ncrabbox-") {
		t.Fatalf("delete not recorded after launch failure:\n%s", deleteArgs)
	}
}

func TestAcquireKeepRetainsKeyAfterPostLaunchInfoFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{
			commandKey([]string{"list", "--format", "json"}): {Stdout: `{"list":[]}`},
			"launch": {Stdout: "launched"},
			"info":   {Stderr: "info failed"},
		},
		errors: map[string]error{"info": errors.New("info failed")},
	}
	b := testBackend(runner)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}, Keep: true})
	if err == nil || !strings.Contains(err.Error(), "multipass info failed") {
		t.Fatalf("Acquire error=%v", err)
	}
	for _, call := range runner.calls {
		if len(call.Args) > 0 && call.Args[0] == "delete" {
			t.Fatalf("kept post-launch failure should not delete instance: %#v", call.Args)
		}
	}
	keys, err := findStoredTestboxKeys(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("kept instance key count=%d, want 1: %#v", len(keys), keys)
	}
}

func findStoredTestboxKeys(root string) ([]string, error) {
	keys := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == "id_ed25519" {
			keys = append(keys, path)
		}
		return nil
	})
	return keys, err
}

func TestAcquireRemovesClaimAfterEndpointUpdateFailure(t *testing.T) {
	runner, b := setupAcquireMetadataFailureTest(t)
	oldUpdate := updateLeaseClaimEndpoint
	updateLeaseClaimEndpoint = func(string, Server, SSHTarget) error {
		return errors.New("endpoint boom")
	}
	t.Cleanup(func() { updateLeaseClaimEndpoint = oldUpdate })
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "endpoint boom") {
		t.Fatalf("Acquire error=%v", err)
	}
	assertAcquireRollbackRemovedInstanceAndClaim(t, runner)
}

func TestAcquireRemovesClaimAfterCacheVolumeUpdateFailure(t *testing.T) {
	runner, b := setupAcquireMetadataFailureTest(t)
	oldUpdate := updateLeaseClaimCacheVolumes
	updateLeaseClaimCacheVolumes = func(string, []string) error {
		return errors.New("cache volume boom")
	}
	t.Cleanup(func() { updateLeaseClaimCacheVolumes = oldUpdate })
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "cache volume boom") {
		t.Fatalf("Acquire error=%v", err)
	}
	assertAcquireRollbackRemovedInstanceAndClaim(t, runner)
}

func TestAcquireKeepsClaimWhenMetadataRollbackDeleteFails(t *testing.T) {
	runner, b := setupAcquireMetadataFailureTest(t)
	oldUpdate := updateLeaseClaimEndpoint
	updateLeaseClaimEndpoint = func(string, Server, SSHTarget) error {
		return errors.New("endpoint boom")
	}
	t.Cleanup(func() { updateLeaseClaimEndpoint = oldUpdate })
	runner.errors = map[string]error{"delete": errors.New("delete boom")}
	_, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "endpoint boom") || !strings.Contains(err.Error(), "multipass cleanup failed") {
		t.Fatalf("Acquire error=%v, want metadata and cleanup errors", err)
	}
	_ = recordedArgsForCommand(t, runner, "delete")
	claims, claimErr := listLeaseClaims()
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if len(claims) != 1 {
		t.Fatalf("claim should remain when instance deletion fails: %#v", claims)
	}
}

func setupAcquireMetadataFailureTest(t *testing.T) (*recordingRunner, *backend) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	oldWait := waitForSSHReady
	waitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return nil
	}
	t.Cleanup(func() { waitForSSHReady = oldWait })
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"list", "--format", "json"}): {Stdout: `{"list":[]}`},
		"launch": {},
		"info":   {Stdout: sampleInfoJSON("{{name}}")},
	}}
	return runner, testBackend(runner)
}

func assertAcquireRollbackRemovedInstanceAndClaim(t *testing.T, runner *recordingRunner) {
	t.Helper()
	deleteArgs := recordedArgsForCommand(t, runner, "delete")
	if !strings.Contains(deleteArgs, "delete\n--purge\ncrabbox-") {
		t.Fatalf("delete not recorded after metadata failure:\n%s", deleteArgs)
	}
	claims, err := listLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("claim was not removed after rollback: %#v", claims)
	}
}

func TestDoctorReportsMissingCLI(t *testing.T) {
	runner := &recordingRunner{
		responses: map[string]core.LocalCommandResult{"version": {Stderr: "command not found", ExitCode: 127}},
		errors:    map[string]error{"version": errors.New("exec: multipass not found")},
	}
	b := testBackend(runner)
	if _, err := b.Doctor(context.Background(), core.DoctorRequest{}); err == nil || !strings.Contains(err.Error(), "multipass version failed") {
		t.Fatalf("doctor error=%v", err)
	}
}

func TestDurationSecondsCeil(t *testing.T) {
	for input, want := range map[time.Duration]int{
		0:                       1,
		1500 * time.Millisecond: 2,
		2 * time.Second:         2,
	} {
		if got := durationSecondsCeil(input); got != want {
			t.Fatalf("%s -> %d want %d", input, got, want)
		}
	}
}

func TestCacheVolumeNameIsStableAndFilesystemSafe(t *testing.T) {
	got := multipassCacheVolumeName("My App/linux node24 lock")
	again := multipassCacheVolumeName("My App/linux node24 lock")
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

func TestLaunchTimeoutFlagParsesDuration(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := registerFlags(fs, core.BaseConfig())
	if err := fs.Parse([]string{"--multipass-launch-timeout", "7m"}); err != nil {
		t.Fatal(err)
	}
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	if err := applyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.Multipass.LaunchTimeout != 7*time.Minute {
		t.Fatalf("launch timeout=%s", cfg.Multipass.LaunchTimeout)
	}
}

func TestNoPrivateKeyMaterialInLaunchArgs(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{"launch": {}}}
	b := testBackend(runner)
	if err := b.createInstance(context.Background(), b.configForRun(), "crabbox-blue-1234abcd", "cbx_123", "blue-lobster", "PUBLIC-KEY"); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "launch")
	if strings.Contains(strings.ToUpper(args), "PRIVATE") {
		t.Fatalf("unexpected secret-like content in args:\n%s", args)
	}
	if strings.Contains(args, "PUBLIC-KEY") {
		t.Fatalf("public key should be in cloud-init file, not process args:\n%s", args)
	}
}

func TestInstanceScopeRoundTrip(t *testing.T) {
	name := "crabbox-blue-1234abcd"
	if got := instanceNameFromScope(instanceScope(name)); got != name {
		t.Fatalf("instance name=%q want %q", got, name)
	}
}

func TestListJSONDecode(t *testing.T) {
	var out listResponse
	if err := json.Unmarshal([]byte(sampleListJSON()), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.List) != 2 || out.List[0].ip() != "192.168.64.7" {
		t.Fatalf("decoded=%#v", out)
	}
}

func TestInfoJSONDecode(t *testing.T) {
	var out infoResponse
	if err := json.Unmarshal([]byte(sampleInfoJSON("crabbox-blue-1234abcd")), &out); err != nil {
		t.Fatal(err)
	}
	inst := out.Info["crabbox-blue-1234abcd"].toInstance("crabbox-blue-1234abcd")
	if inst.Name != "crabbox-blue-1234abcd" || inst.Release != "Ubuntu 24.04.4 LTS" || inst.ip() != "192.168.64.7" {
		t.Fatalf("decoded=%#v", inst)
	}
}

func TestCacheVolumeMountValidation(t *testing.T) {
	if _, err := multipassCacheVolumeMounts([]core.CacheVolumeConfig{{Key: "bad:key", Path: "/cache"}}); err == nil {
		t.Fatal("accepted ':' in cache key")
	}
	if _, err := multipassCacheVolumeMounts([]core.CacheVolumeConfig{{Key: "ok", Path: "relative"}}); err == nil {
		t.Fatal("accepted relative cache path")
	}
}

func TestLaunchTimeoutArgumentRoundsUp(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{"launch": {}}}
	b := testBackend(runner)
	cfg := b.configForRun()
	cfg.Multipass.LaunchTimeout = 1500 * time.Millisecond
	if err := b.createInstance(context.Background(), cfg, "crabbox-blue-1234abcd", "cbx_123", "blue-lobster", "PUB"); err != nil {
		t.Fatal(err)
	}
	args := recordedArgsForCommand(t, runner, "launch")
	if !strings.Contains(args, "--timeout\n2") {
		t.Fatalf("timeout arg not rounded up:\n%s", args)
	}
}

func TestServerFromUnclaimedCrabboxNamedInstance(t *testing.T) {
	b := testBackend(&recordingRunner{})
	server := b.serverFromInstance(multipassInstance{Name: "crabbox-blue-1234abcd", State: "Running", IPv4: []string{"192.168.64.7"}, Release: "Ubuntu 24.04 LTS"}, core.LeaseClaim{}, b.configForRun())
	if server.CloudID != "crabbox-blue-1234abcd" || server.Labels["provider"] != providerName || server.Labels["instance"] != "crabbox-blue-1234abcd" {
		t.Fatalf("server=%#v", server)
	}
}

func TestShouldCleanupRespectsKeepLabel(t *testing.T) {
	server := Server{Status: "stopped", Labels: map[string]string{"keep": "true"}}
	if ok, reason := shouldCleanup(server, core.LeaseClaim{}, true, time.Now()); ok || reason != "keep=true" {
		t.Fatalf("cleanup=%v reason=%s", ok, reason)
	}
}

func TestShouldCleanupExpiredClaim(t *testing.T) {
	server := Server{Status: "running", Labels: map[string]string{}}
	claim := core.LeaseClaim{LeaseID: "cbx_123", LastUsedAt: time.Now().Add(-48 * time.Hour).Format(time.RFC3339), IdleTimeoutSeconds: int((30 * time.Minute).Seconds())}
	if ok, reason := shouldCleanup(server, claim, true, time.Now()); !ok || reason != "claim expired" {
		t.Fatalf("cleanup=%v reason=%s", ok, reason)
	}
}

func TestShouldCleanupSkipsMissingClaim(t *testing.T) {
	server := Server{Status: "running", Labels: map[string]string{}}
	if ok, reason := shouldCleanup(server, core.LeaseClaim{}, false, time.Now()); ok || reason != "missing claim" {
		t.Fatalf("cleanup=%v reason=%s", ok, reason)
	}
}

func TestLaunchArgTimeoutValueIsNumeric(t *testing.T) {
	if _, err := strconv.Atoi(strconv.Itoa(durationSecondsCeil(10 * time.Minute))); err != nil {
		t.Fatal(err)
	}
}
