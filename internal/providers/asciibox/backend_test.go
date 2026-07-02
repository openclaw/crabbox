package asciibox

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderSpecAndAliases(t *testing.T) {
	p := Provider{}
	if p.Name() != providerName {
		t.Fatalf("Name=%q want %s", p.Name(), providerName)
	}
	for _, alias := range []string{"ascii", "asciibox", "ascii-box"} {
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
	if spec.Coordinator != core.CoordinatorNever {
		t.Fatalf("coordinator=%v want never", spec.Coordinator)
	}
	if len(spec.Targets) != 1 || spec.Targets[0].OS != core.TargetLinux {
		t.Fatalf("targets=%#v want linux", spec.Targets)
	}
	if !hasFeature(spec.Features, core.FeatureSSH) || !hasFeature(spec.Features, core.FeatureCrabboxSync) {
		t.Fatalf("features=%#v want ssh and crabbox sync", spec.Features)
	}
}

func TestClientUsesOfficialAsciiBoxCLI(t *testing.T) {
	t.Setenv("BOX_API_KEY", "stale_key")
	home := t.TempDir()
	runner := &fakeCommandRunner{configPath: home + "/Library/Application Support/ascii/box/config.json"}
	client := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: home, runner: runner}
	box, err := client.CreateBox(context.Background(), createRequest{TTL: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if box.ID != "bx_1" || boxHost(box) != "203.0.113.10" || boxSSHUser(box) != "user" {
		t.Fatalf("box=%#v", box)
	}
	if err := client.PrepareSSH(context.Background(), "bx_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetBox(context.Background(), "bx_1"); err != nil {
		t.Fatal(err)
	}
	if boxes, err := client.ListBoxes(context.Background()); err != nil || len(boxes) != 1 {
		t.Fatalf("boxes=%#v err=%v", boxes, err)
	}
	if err := client.ReleaseBox(context.Background(), "bx_1"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"box --no-update --json --api-url https://ascii.dev status",
		"box --no-update --json --api-url https://ascii.dev new --ttl 1800",
		"box --no-update --json --api-url https://ascii.dev status",
		"box --no-update --json --api-url https://ascii.dev ssh bx_1 -- true",
		"box --no-update --json --api-url https://ascii.dev status",
		"box --no-update --json --api-url https://ascii.dev info bx_1",
		"box --no-update --json --api-url https://ascii.dev status",
		"box --no-update --json --api-url https://ascii.dev list",
		"box --no-update --json --api-url https://ascii.dev status",
		"box --no-update --json --api-url https://ascii.dev stop bx_1",
		"box --no-update --json --api-url https://ascii.dev delete bx_1",
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands=%v want=%v", runner.commands, want)
	}
	for _, env := range runner.env {
		if !hasEnv(env, "BOX_API_KEY=box_key") {
			t.Fatalf("env missing BOX_API_KEY: %v", env)
		}
		if hasEnv(env, "BOX_API_KEY=stale_key") {
			t.Fatalf("env kept stale BOX_API_KEY: %v", env)
		}
		if !hasEnv(env, "HOME="+home) {
			t.Fatalf("env missing HOME: %v", env)
		}
	}
	if !hasEnv(runner.env[3], "SSH_AUTH_SOCK=") {
		t.Fatalf("ssh setup env should disable agent identities: %v", runner.env[3])
	}
}

func TestReleaseBoxRecoversFromRecentSnapshotGuard(t *testing.T) {
	runner := &releaseCommandRunner{
		configPath: filepath.Join(t.TempDir(), "config.json"),
		outcomes: map[string][]commandOutcome{
			"stop":   {snapshotGuardOutcome()},
			"delete": {snapshotGuardOutcome(), {result: LocalCommandResult{Stdout: `{"id":"bx_guard","status":"deleted"}`}}},
			"extend": {{result: LocalCommandResult{Stdout: `{"id":"bx_guard","archiveAfter":"soon"}`}}},
			"info": {
				{result: LocalCommandResult{Stdout: `{"box":{"id":"bx_guard","state":"idle"}}`}},
				{result: LocalCommandResult{Stdout: `{"box":{"id":"bx_guard","state":"idle","status":"stopping"}}`}},
			},
		},
	}
	client := &client{
		apiKey:              "box_key",
		apiURL:              "https://ascii.dev",
		cliPath:             "box",
		home:                t.TempDir(),
		runner:              runner,
		releasePollInterval: time.Nanosecond,
	}

	if err := client.ReleaseBox(context.Background(), "bx_guard"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"box --no-update --json --api-url https://ascii.dev status",
		"box --no-update --json --api-url https://ascii.dev stop bx_guard",
		"box --no-update --json --api-url https://ascii.dev delete bx_guard",
		"box --no-update --json --api-url https://ascii.dev extend bx_guard --ttl 1",
		"box --no-update --json --api-url https://ascii.dev info bx_guard",
		"box --no-update --json --api-url https://ascii.dev info bx_guard",
		"box --no-update --json --api-url https://ascii.dev delete bx_guard",
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands=%v want=%v", runner.commands, want)
	}
}

func TestReleaseBoxDoesNotRecoverUnrelatedDeleteFailure(t *testing.T) {
	runner := &releaseCommandRunner{
		configPath: filepath.Join(t.TempDir(), "config.json"),
		outcomes: map[string][]commandOutcome{
			"stop":   {{result: LocalCommandResult{}}},
			"delete": {{result: LocalCommandResult{Stderr: "permission denied"}, err: fmt.Errorf("exit status 1")}},
		},
	}
	client := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: t.TempDir(), runner: runner}

	err := client.ReleaseBox(context.Background(), "bx_guard")
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("ReleaseBox err=%v", err)
	}
	if containsCommand(runner.commands, "box --no-update --json --api-url https://ascii.dev extend bx_guard --ttl 1") {
		t.Fatalf("unexpected snapshot recovery commands=%v", runner.commands)
	}
}

func TestReleaseBoxReportsSnapshotRecoveryExtendFailure(t *testing.T) {
	runner := &releaseCommandRunner{
		configPath: filepath.Join(t.TempDir(), "config.json"),
		outcomes: map[string][]commandOutcome{
			"stop":   {snapshotGuardOutcome()},
			"delete": {snapshotGuardOutcome()},
			"extend": {{result: LocalCommandResult{Stderr: "extend throttled"}, err: fmt.Errorf("exit status 1")}},
		},
	}
	client := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: t.TempDir(), runner: runner}

	err := client.ReleaseBox(context.Background(), "bx_guard")
	if err == nil || !strings.Contains(err.Error(), "snapshot recovery extend: extend throttled") {
		t.Fatalf("ReleaseBox err=%v", err)
	}
}

func TestReleaseBoxSkipsSnapshotRecoveryAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &releaseCommandRunner{
		configPath: filepath.Join(t.TempDir(), "config.json"),
		outcomes: map[string][]commandOutcome{
			"stop":   {snapshotGuardOutcome()},
			"delete": {snapshotGuardOutcome()},
		},
		onAction: func(action string) {
			if action == "delete" {
				cancel()
			}
		},
	}
	client := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: t.TempDir(), runner: runner}

	err := client.ReleaseBox(ctx, "bx_guard")
	if err == nil || !strings.Contains(err.Error(), "snapshot recovery: context canceled") {
		t.Fatalf("ReleaseBox err=%v", err)
	}
	if containsCommand(runner.commands, "box --no-update --json --api-url https://ascii.dev extend bx_guard --ttl 1") {
		t.Fatalf("unexpected snapshot recovery commands=%v", runner.commands)
	}
}

func TestAsciiBoxBaseURLValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "canonical https", raw: "HTTPS://ASCII.DEV:443/", want: "https://ascii.dev"},
		{name: "https path", raw: "https://ascii.dev/api/", want: "https://ascii.dev/api"},
		{name: "escaped path", raw: "https://ascii.dev/tenant%2F/", want: "https://ascii.dev/tenant%2F"},
		{name: "localhost", raw: "http://localhost:8080/", want: "http://localhost:8080"},
		{name: "ipv4 loopback", raw: "http://127.0.0.2:8080/api", want: "http://127.0.0.2:8080/api"},
		{name: "ipv6 loopback", raw: "http://[::1]:80/", want: "http://[::1]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := validateAsciiBoxBaseURL(test.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("url=%q want %q", got, test.want)
			}
		})
	}

	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "public http", raw: "http://ascii.dev"},
		{name: "relative", raw: "/api"},
		{name: "schemeless", raw: "ascii.dev"},
		{name: "missing host", raw: "https:///api"},
		{name: "opaque", raw: "https:ascii.dev"},
		{name: "other scheme", raw: "ftp://ascii.dev"},
		{name: "userinfo", raw: "https://token@ascii.dev"},
		{name: "query", raw: "https://ascii.dev?token=1"},
		{name: "bare query", raw: "https://ascii.dev?"},
		{name: "fragment", raw: "https://ascii.dev#fragment"},
		{name: "malformed port", raw: "https://ascii.dev:bad"},
		{name: "loopback lookalike", raw: "http://localhost.example.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateAsciiBoxBaseURL(test.raw); err == nil {
				t.Fatalf("expected %q to be rejected", test.raw)
			}
		})
	}
}

func TestNewAPIRejectsUnsafeBaseURLBeforeCommandOrConfigWrite(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.json")
	runner := &fakeCommandRunner{configPath: configPath}
	cfg := testConfig()
	cfg.AsciiBox.BaseURL = "http://ascii.dev"

	client, err := newAPI(cfg, Runtime{Exec: runner})
	if err == nil || client != nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("client=%#v err=%v", client, err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("commands=%v", runner.commands)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("config path exists or returned unexpected error: %v", err)
	}
}

func TestNewAPICanonicalizesBaseURL(t *testing.T) {
	cfg := testConfig()
	cfg.AsciiBox.BaseURL = " HTTPS://ASCII.DEV:443/api/ "
	got, err := newAPI(cfg, Runtime{Exec: &fakeCommandRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if got.(*client).apiURL != "https://ascii.dev/api" {
		t.Fatalf("apiURL=%q", got.(*client).apiURL)
	}
}

func TestClientTightensExistingConfigFilePermissions(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "Library/Application Support/ascii/box/config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"token":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &fakeCommandRunner{configPath: configPath}
	client := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: home, runner: runner}
	if _, err := client.CreateBox(context.Background(), createRequest{TTL: 30 * time.Minute}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config permissions=%#o, want 0600", got)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]string
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config is invalid JSON: %v", err)
	}
	if cfg["token"] != "box_key" {
		t.Fatalf("token=%q, want box_key", cfg["token"])
	}
}

func TestClientRejectsSymlinkConfigFile(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "Library/Application Support/ascii/box/config.json")
	targetPath := filepath.Join(home, "target.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte(`{"token":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, configPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	runner := &fakeCommandRunner{configPath: configPath}
	client := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: home, runner: runner}
	if _, err := client.CreateBox(context.Background(), createRequest{TTL: 30 * time.Minute}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("CreateBox err=%v, want symlink rejection", err)
	}
}

func TestClientPollsPartialCreateOutput(t *testing.T) {
	home := t.TempDir()
	runner := &fakeCommandRunner{
		configPath: home + "/Library/Application Support/ascii/box/config.json",
		newStdout: strings.Join([]string{
			`{"event":"created","id":"bx_2","ttlSeconds":1800}`,
			`{"event":"state","id":"bx_2","state":"provisioning"}`,
		}, "\n"),
		newErr:        fmt.Errorf("exit status 1"),
		infoResponses: []string{`{"box":{"id":"bx_2","state":"ready","ip":"203.0.113.20","expiresAt":"2026-06-10T12:00:00Z"}}`},
	}
	client := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: home, runner: runner}
	box, err := client.CreateBox(context.Background(), createRequest{TTL: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if box.ID != "bx_2" || boxHost(box) != "203.0.113.20" {
		t.Fatalf("box=%#v", box)
	}
	if got := boxExpiresAt(box); got != "2026-06-10T12:00:00Z" {
		t.Fatalf("boxExpiresAt=%q, want info response expiration", got)
	}
	if !containsCommand(runner.commands, "box --no-update --json --api-url https://ascii.dev info bx_2") {
		t.Fatalf("commands missing info poll: %v", runner.commands)
	}
}

func TestClientPreservesPartialCreateOnErrorEvent(t *testing.T) {
	home := t.TempDir()
	runner := &fakeCommandRunner{
		configPath: home + "/Library/Application Support/ascii/box/config.json",
		newStdout: strings.Join([]string{
			`{"event":"created","id":"bx_3","ttlSeconds":1800}`,
			`{"event":"error","id":"bx_3","message":"open https://box.ascii.dev/session?box_token=secret-value&ok=1"}`,
		}, "\n"),
	}
	client := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: home, runner: runner}
	box, err := client.CreateBox(context.Background(), createRequest{TTL: 30 * time.Minute})
	if err == nil {
		t.Fatal("CreateBox succeeded, want error")
	}
	if box.ID != "bx_3" {
		t.Fatalf("box=%#v, want partial bx_3", box)
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("error leaked box token: %v", err)
	}
}

func TestRedactBoxSecrets(t *testing.T) {
	got := redactBoxSecrets(`open https://box.ascii.dev/session?box_token=secret-value&ok=1 with box_realToken`)
	if strings.Contains(got, "secret-value") || strings.Contains(got, "box_realToken") {
		t.Fatalf("redacted=%q", got)
	}
}

func TestAcquireClaimsBoxAndReturnsSSHTarget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{box: testBox()}
	withFakeAPI(t, fake)
	stubSSHWait(t)

	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	lease, err := backend.Acquire(context.Background(), AcquireRequest{
		Repo:          core.Repo{Name: "repo", Root: t.TempDir()},
		Options:       core.LeaseOptions{TTL: 45 * time.Minute},
		Keep:          true,
		RequestedSlug: "proof",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.SSH.Host != "203.0.113.10" || lease.SSH.User != "user" {
		t.Fatalf("lease=%#v", lease)
	}
	if !strings.HasSuffix(lease.SSH.Key, ".ssh/ascii_box_ed25519") {
		t.Fatalf("ssh key=%q", lease.SSH.Key)
	}
	if !lease.SSH.NoControlMaster {
		t.Fatalf("ascii-box SSH target should disable ControlMaster")
	}
	if fake.createReq.TTL != 45*time.Minute {
		t.Fatalf("create req=%#v", fake.createReq)
	}
	if !reflect.DeepEqual(fake.prepareIDs, []string{"bx_1"}) {
		t.Fatalf("prepare ids=%v", fake.prepareIDs)
	}
	claim, ok, err := core.ResolveLeaseClaim(lease.LeaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.Provider != providerName || claim.ProviderScope != "box:bx_1" || claim.Slug != "proof" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestAcquireReleasesPartiallyCreatedBox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{
		box:       boxData{ID: "bx_partial"},
		createErr: fmt.Errorf("create failed"),
	}
	withFakeAPI(t, fake)

	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	_, err := backend.Acquire(context.Background(), AcquireRequest{
		Repo: core.Repo{Name: "repo", Root: t.TempDir()},
		Keep: true,
	})
	if err == nil {
		t.Fatal("Acquire succeeded, want error")
	}
	if !reflect.DeepEqual(fake.deletedIDs, []string{"bx_partial"}) {
		t.Fatalf("deleted=%v, want [bx_partial]", fake.deletedIDs)
	}
}

func TestResolveUsesClaimScopeAndReleaseDeletesBox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{box: testBox()}
	withFakeAPI(t, fake)
	stubSSHWait(t)
	if err := claimLeaseForRepoProviderScope("cbx_123456789abc", "proof", providerName, "box:bx_1", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}

	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "proof"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_123456789abc" || lease.Server.CloudID != "bx_1" || lease.SSH.Host != "203.0.113.10" {
		t.Fatalf("lease=%#v", lease)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.deletedIDs, []string{"bx_1"}) {
		t.Fatalf("deleted=%v", fake.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaim("proof"); err != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed", ok, err)
	}
}

func TestResolveReleaseOnlyDoesNotRequireSSHFields(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{box: boxData{ID: "bx_booting", Status: "provisioning"}}
	withFakeAPI(t, fake)
	if err := claimLeaseForRepoProviderScope("cbx_abcdef123456", "booting", providerName, "box:bx_booting", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}

	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "booting", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "" || lease.Server.CloudID != "bx_booting" {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestResolveRawBoxIDClaimsProviderScope(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{box: boxData{ID: "bx_external", State: "ready", IP: "203.0.113.30"}}
	withFakeAPI(t, fake)
	stubSSHWait(t)

	repoRoot := t.TempDir()
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "bx_external", Repo: core.Repo{Root: repoRoot}})
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := core.ResolveLeaseClaim(lease.LeaseID)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.ProviderScope != "box:bx_external" {
		t.Fatalf("provider scope=%q", claim.ProviderScope)
	}
	resolved, err := backend.Resolve(context.Background(), ResolveRequest{ID: lease.LeaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Server.CloudID != "bx_external" {
		t.Fatalf("resolved=%#v", resolved)
	}
}

func TestStatusMapsBoxAPIFields(t *testing.T) {
	fake := &fakeAPI{box: testBox()}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	view, err := backend.Status(context.Background(), StatusRequest{ID: "bx_1"})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "ascii_bx_1" || view.ServerID != "bx_1" || view.SSHHost != "203.0.113.10" || view.SSHUser != "user" || !view.Ready {
		t.Fatalf("view=%#v", view)
	}
}

func TestStatusWaitReturnsTerminalBoxState(t *testing.T) {
	fake := &fakeAPI{box: boxData{ID: "bx_failed", State: "error", IP: "203.0.113.10"}}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	view, err := backend.Status(context.Background(), StatusRequest{ID: "bx_failed", Wait: true, WaitTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "error" || view.Ready {
		t.Fatalf("view=%#v", view)
	}
}

func TestCleanWorkdirAndFlags(t *testing.T) {
	if got, err := cleanWorkdir(" /home/user/crabbox/ "); err != nil || got != "/home/user/crabbox" {
		t.Fatalf("workdir=%q err=%v", got, err)
	}
	for _, value := range []string{"", "repo", "/", "/home/user", "/workspace", "/tmp"} {
		if _, err := cleanWorkdir(value); err == nil {
			t.Fatalf("cleanWorkdir(%q) succeeded", value)
		}
	}

	cfg := testConfig()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	values := RegisterAsciiBoxProviderFlags(fs, cfg)
	if err := fs.Parse([]string{"--ascii-box-cli", "/tmp/box", "--ascii-box-workdir", "/home/user/project"}); err != nil {
		t.Fatal(err)
	}
	if err := ApplyAsciiBoxProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	if cfg.AsciiBox.CLIPath != "/tmp/box" || cfg.WorkRoot != "/home/user/project" || cfg.AsciiBox.Workdir != "/home/user/project" {
		t.Fatalf("cfg=%#v", cfg)
	}
}

func hasFeature(features core.FeatureSet, want core.Feature) bool {
	for _, feature := range features {
		if feature == want {
			return true
		}
	}
	return false
}

func testConfig() Config {
	return Config{
		Provider: providerName,
		SSHKey:   "/tmp/global-crabbox-key",
		AsciiBox: AsciiBoxConfig{
			APIKey:  "box_key",
			BaseURL: "https://ascii.dev",
			CLIPath: "box",
			Workdir: "/home/user/crabbox",
		},
	}
}

func testRuntime() Runtime {
	return Runtime{Stdout: io.Discard, Stderr: io.Discard}
}

func testBox() boxData {
	return boxData{ID: "bx_1", State: "ready", IP: "203.0.113.10"}
}

func withFakeAPI(t *testing.T, fake *fakeAPI) {
	t.Helper()
	original := newAPI
	newAPI = func(Config, Runtime) (api, error) { return fake, nil }
	t.Cleanup(func() { newAPI = original })
}

func stubSSHWait(t *testing.T) {
	t.Helper()
	original := waitForSSHReadyFunc
	waitForSSHReadyFunc = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error { return nil }
	t.Cleanup(func() { waitForSSHReadyFunc = original })
}

type fakeAPI struct {
	createReq  createRequest
	createErr  error
	box        boxData
	prepareIDs []string
	deletedIDs []string
}

func (f *fakeAPI) CreateBox(_ context.Context, req createRequest) (boxData, error) {
	f.createReq = req
	if f.box.ID == "" {
		f.box = testBox()
	}
	if f.createErr != nil {
		return f.box, f.createErr
	}
	return f.box, nil
}

func (f *fakeAPI) Check(context.Context) error { return nil }

func (f *fakeAPI) PrepareSSH(_ context.Context, id string) error {
	f.prepareIDs = append(f.prepareIDs, id)
	return nil
}

func (f *fakeAPI) GetBox(_ context.Context, id string) (boxData, error) {
	if f.box.ID == "" {
		f.box = testBox()
	}
	if id != f.box.ID {
		return boxData{}, fmt.Errorf("404 not found")
	}
	return f.box, nil
}

func (f *fakeAPI) ListBoxes(context.Context) ([]boxData, error) {
	if f.box.ID == "" {
		f.box = testBox()
	}
	return []boxData{f.box}, nil
}

func (f *fakeAPI) ReleaseBox(_ context.Context, id string) error {
	f.deletedIDs = append(f.deletedIDs, id)
	return nil
}

type fakeCommandRunner struct {
	commands   []string
	env        [][]string
	configPath string
	newStdout  string
	newErr     error

	infoResponses []string
}

type commandOutcome struct {
	result LocalCommandResult
	err    error
}

type releaseCommandRunner struct {
	commands   []string
	configPath string
	outcomes   map[string][]commandOutcome
	onAction   func(string)
}

func (r *releaseCommandRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.commands = append(r.commands, strings.Join(append([]string{req.Name}, req.Args...), " "))
	action := boxCLIAction(req.Args)
	if action == "status" {
		return LocalCommandResult{Stdout: fmt.Sprintf(`{"account":null,"api":{},"config":{"path":%q}}`, r.configPath)}, nil
	}
	queue := r.outcomes[action]
	if len(queue) == 0 {
		return LocalCommandResult{Stderr: "unexpected command"}, fmt.Errorf("unexpected %s command", action)
	}
	outcome := queue[0]
	r.outcomes[action] = queue[1:]
	if r.onAction != nil {
		r.onAction(action)
	}
	return outcome.result, outcome.err
}

func boxCLIAction(args []string) string {
	for _, arg := range args {
		switch arg {
		case "status", "stop", "delete", "extend", "info":
			return arg
		}
	}
	return ""
}

func snapshotGuardOutcome() commandOutcome {
	return commandOutcome{
		result: LocalCommandResult{Stdout: `{"code":"snapshot_required","error":"Refusing request: no successful snapshot in the last 30 minutes.","status":409}`},
		err:    fmt.Errorf("exit status 1"),
	}
}

func (r *fakeCommandRunner) Run(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	r.commands = append(r.commands, strings.Join(append([]string{req.Name}, req.Args...), " "))
	r.env = append(r.env, req.Env)
	joined := strings.Join(req.Args, " ")
	switch {
	case strings.Contains(joined, " status"):
		return LocalCommandResult{Stdout: fmt.Sprintf(`{"account":null,"api":{},"config":{"path":%q}}`, r.configPath)}, nil
	case strings.Contains(joined, " new "):
		if r.newStdout != "" || r.newErr != nil {
			return LocalCommandResult{Stdout: r.newStdout}, r.newErr
		}
		return LocalCommandResult{Stdout: strings.Join([]string{
			`{"event":"created","id":"bx_1","ttlSeconds":1800}`,
			`{"event":"state","id":"bx_1","state":"provisioning"}`,
			`{"event":"ready","id":"bx_1","state":"ready","ip":"203.0.113.10","archiveAfter":"2026-05-30T20:00:00Z"}`,
		}, "\n")}, nil
	case strings.Contains(joined, " ssh bx_1 -- true"):
		return LocalCommandResult{}, nil
	case strings.Contains(joined, " info bx_1"):
		return LocalCommandResult{Stdout: `{"box":{"id":"bx_1","state":"ready","ip":"203.0.113.10"}}`}, nil
	case strings.Contains(joined, " info bx_2"):
		if len(r.infoResponses) == 0 {
			return LocalCommandResult{Stderr: "missing info response"}, fmt.Errorf("missing info response")
		}
		out := r.infoResponses[0]
		r.infoResponses = r.infoResponses[1:]
		return LocalCommandResult{Stdout: out}, nil
	case strings.Contains(joined, " list"):
		return LocalCommandResult{Stdout: `{"boxes":[{"id":"bx_1","state":"ready","ip":"203.0.113.10"}]}`}, nil
	case strings.Contains(joined, " stop bx_1"):
		return LocalCommandResult{Stdout: `{"id":"bx_1","status":"deleted"}`}, nil
	case strings.Contains(joined, " delete bx_1"):
		return LocalCommandResult{Stdout: `{"id":"bx_1","status":"deleted"}`}, nil
	default:
		return LocalCommandResult{Stderr: "unexpected command"}, fmt.Errorf("unexpected command")
	}
}

func hasEnv(env []string, want string) bool {
	for _, value := range env {
		if value == want {
			return true
		}
	}
	return false
}

func containsCommand(commands []string, want string) bool {
	for _, command := range commands {
		if command == want {
			return true
		}
	}
	return false
}
