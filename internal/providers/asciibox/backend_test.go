package asciibox

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestManualConfigInputFlags(t *testing.T) {
	cfg := core.BaseConfig()
	cfg.Provider = "fixture-other"
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	values := RegisterAsciiBoxProviderFlags(fs, cfg)
	before := cfg
	if err := ApplyAsciiBoxProviderFlags(&cfg, fs, struct{}{}); err != nil || !reflect.DeepEqual(cfg, before) {
		t.Fatalf("foreign values changed configuration: %v", err)
	}
	if err := ApplyAsciiBoxProviderFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	want := cfg
	core.RecordProviderFlagInputs(&want, true, "ascii-box")
	if reflect.DeepEqual(cfg, want) {
		t.Fatal("unvisited flags recorded input")
	}
	for repeat := 0; repeat < 2; repeat++ {
		if err := fs.Set("ascii-box-workdir", "/work/fixture"); err != nil {
			t.Fatal(err)
		}
		if err := ApplyAsciiBoxProviderFlags(&cfg, fs, values); err != nil {
			t.Fatal(err)
		}
		want = cfg
		core.RecordProviderFlagInputs(&want, true, "ascii-box")
		if !reflect.DeepEqual(cfg, want) {
			t.Fatal("accepted/equal flag value was not recorded")
		}
	}
}

func TestMain(m *testing.M) {
	os.Exit(testutil.RunWithIsolatedUserDirs(m))
}

func TestProviderSpecAndAliases(t *testing.T) {
	p := Provider{}
	if p.Spec().Name != providerName {
		t.Fatalf("Name=%q want %s", p.Spec().Name, providerName)
	}
	for _, alias := range []string{"boat", "ascii", "asciibox", "ascii-box"} {
		got, err := core.ProviderFor(alias)
		if err != nil {
			t.Fatalf("ProviderFor(%q): %v", alias, err)
		}
		if got.Spec().Name != providerName {
			t.Fatalf("ProviderFor(%q).Name=%q", alias, got.Spec().Name)
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
	if boxes, err := client.ListBoxes(context.Background(), false); err != nil || len(boxes) != 1 {
		t.Fatalf("boxes=%#v err=%v", boxes, err)
	}
	if err := client.ReleaseBox(context.Background(), "bx_1", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"box --no-update --json --org personal --api-url https://ascii.dev status",
		"box --no-update --json --org personal --api-url https://ascii.dev new --ttl 1800",
		"box --no-update --json --org personal --api-url https://ascii.dev status",
		"box --no-update --json --org personal --api-url https://ascii.dev ssh bx_1 -- true",
		"box --no-update --json --org personal --api-url https://ascii.dev status",
		"box --no-update --json --org personal --api-url https://ascii.dev info bx_1",
		"box --no-update --json --org personal --api-url https://ascii.dev status",
		"box --no-update --json --org personal --api-url https://ascii.dev list --all",
		"box --no-update --json --org personal --api-url https://ascii.dev status",
		"box --no-update --json --org personal --api-url https://ascii.dev stop bx_1",
		"box --no-update --json --org personal --api-url https://ascii.dev delete bx_1 --yes",
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands=%v want=%v", runner.commands, want)
	}
	for _, env := range runner.env {
		if !hasEnv(env, "BOX_API_KEY=box_key") {
			t.Fatal("child environment missing the synthetic BOX_API_KEY")
		}
		if hasEnv(env, "BOX_API_KEY=stale_key") {
			t.Fatal("child environment retained the synthetic stale BOX_API_KEY")
		}
		if !hasEnv(env, "HOME="+home) {
			t.Fatal("child environment missing the isolated HOME")
		}
	}
	if !hasEnv(runner.env[3], "SSH_AUTH_SOCK=") {
		t.Fatal("SSH setup must disable agent identities")
	}
}

func TestReleaseBoxRecoversFromRecentSnapshotGuard(t *testing.T) {
	runner := &releaseCommandRunner{
		configPath: filepath.Join(t.TempDir(), "config.json"),
		outcomes: map[string][]commandOutcome{
			"stop":     {snapshotGuardOutcome()},
			"delete":   {snapshotGuardOutcome(), deletionOutcome(testDeletionID, "bx_guard", "box", "pending")},
			"deletion": {deletionOutcome(testDeletionID, "bx_guard", "box", "completed")},
			"extend":   {{result: core.LocalCommandResult{Stdout: `{"id":"bx_guard","archiveAfter":"soon"}`}}},
			"info": {
				{result: core.LocalCommandResult{Stdout: `{"box":{"id":"bx_guard","state":"idle"}}`}},
				{result: core.LocalCommandResult{Stdout: `{"box":{"id":"bx_guard","state":"idle","status":"stopping"}}`}},
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

	if err := client.ReleaseBox(context.Background(), "bx_guard", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"box --no-update --json --org personal --api-url https://ascii.dev status",
		"box --no-update --json --org personal --api-url https://ascii.dev stop bx_guard",
		"box --no-update --json --org personal --api-url https://ascii.dev delete bx_guard --yes",
		"box --no-update --json --org personal --api-url https://ascii.dev extend bx_guard --ttl 1",
		"box --no-update --json --org personal --api-url https://ascii.dev info bx_guard",
		"box --no-update --json --org personal --api-url https://ascii.dev info bx_guard",
		"box --no-update --json --org personal --api-url https://ascii.dev delete bx_guard --yes",
		"box --no-update --json --org personal --api-url https://ascii.dev status",
		"box --no-update --json --org personal --api-url https://ascii.dev deletion status " + testDeletionID,
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands=%v want=%v", runner.commands, want)
	}
}

func TestReleaseBoxDoesNotRecoverUnrelatedDeleteFailure(t *testing.T) {
	runner := &releaseCommandRunner{
		configPath: filepath.Join(t.TempDir(), "config.json"),
		outcomes: map[string][]commandOutcome{
			"stop":   {{result: core.LocalCommandResult{}}},
			"delete": {{result: core.LocalCommandResult{Stderr: "permission denied"}, err: fmt.Errorf("exit status 1")}},
		},
	}
	client := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: t.TempDir(), runner: runner}

	err := client.ReleaseBox(context.Background(), "bx_guard", func(context.Context) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("ReleaseBox err=%v", err)
	}
	if containsCommand(runner.commands, "box --no-update --json --org personal --api-url https://ascii.dev extend bx_guard --ttl 1") {
		t.Fatalf("unexpected snapshot recovery commands=%v", runner.commands)
	}
}

func TestReleaseBoxReportsSnapshotRecoveryExtendFailure(t *testing.T) {
	runner := &releaseCommandRunner{
		configPath: filepath.Join(t.TempDir(), "config.json"),
		outcomes: map[string][]commandOutcome{
			"stop":   {snapshotGuardOutcome()},
			"delete": {snapshotGuardOutcome()},
			"extend": {{result: core.LocalCommandResult{Stderr: "extend throttled"}, err: fmt.Errorf("exit status 1")}},
		},
	}
	client := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: t.TempDir(), runner: runner}

	err := client.ReleaseBox(context.Background(), "bx_guard", func(context.Context) error { return nil })
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

	err := client.ReleaseBox(ctx, "bx_guard", func(context.Context) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "snapshot recovery: context canceled") {
		t.Fatalf("ReleaseBox err=%v", err)
	}
	if containsCommand(runner.commands, "box --no-update --json --org personal --api-url https://ascii.dev extend bx_guard --ttl 1") {
		t.Fatalf("unexpected snapshot recovery commands=%v", runner.commands)
	}
}

const testDeletionID = "bdop_0123456789abcdef0123456789abcdef"

func deletionOutcome(id, target, kind, state string) commandOutcome {
	completedAt := "null"
	if state == "completed" {
		completedAt = `"2026-09-02T09:00:00Z"`
	}
	return commandOutcome{result: core.LocalCommandResult{Stdout: fmt.Sprintf(`{"operation":{"id":%q,"targetId":%q,"kind":%q,"status":%q,"completedAt":%s}}`, id, target, kind, state, completedAt)}}
}

func TestReleaseBoxRequiresCompletedDeletionOperation(t *testing.T) {
	for _, test := range []struct {
		name     string
		initial  commandOutcome
		polls    []commandOutcome
		cancelOn string
		wantErr  bool
	}{
		{name: "pending processing completed", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), polls: []commandOutcome{deletionOutcome(testDeletionID, "bx_guard", "box", "processing"), deletionOutcome(testDeletionID, "bx_guard", "box", "completed")}},
		{name: "blocked completed", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "blocked"), polls: []commandOutcome{deletionOutcome(testDeletionID, "bx_guard", "box", "completed")}},
		{name: "already completed", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "completed")},
		{name: "missing receipt", initial: commandOutcome{result: core.LocalCommandResult{Stdout: `{}`}}, wantErr: true},
		{name: "legacy deleted response", initial: commandOutcome{result: core.LocalCommandResult{Stdout: `{"id":"bx_guard","status":"deleted"}`}}, wantErr: true},
		{name: "malformed receipt", initial: commandOutcome{result: core.LocalCommandResult{Stdout: `not json`}}, wantErr: true},
		{name: "invalid operation ID", initial: deletionOutcome("bdop_other", "bx_guard", "box", "completed"), wantErr: true},
		{name: "wrong initial target", initial: deletionOutcome(testDeletionID, "bx_other", "box", "completed"), wantErr: true},
		{name: "wrong initial kind", initial: deletionOutcome(testDeletionID, "bx_guard", "account", "completed"), wantErr: true},
		{name: "unknown state", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "deleted"), wantErr: true},
		{name: "missing completion timestamp", initial: commandOutcome{result: core.LocalCommandResult{Stdout: fmt.Sprintf(`{"operation":{"id":%q,"targetId":"bx_guard","kind":"box","status":"completed","completedAt":null}}`, testDeletionID)}}, wantErr: true},
		{name: "invalid completion timestamp", initial: commandOutcome{result: core.LocalCommandResult{Stdout: fmt.Sprintf(`{"operation":{"id":%q,"targetId":"bx_guard","kind":"box","status":"completed","completedAt":"yesterday"}}`, testDeletionID)}}, wantErr: true},
		{name: "changed operation", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), polls: []commandOutcome{deletionOutcome("bdop_fedcba9876543210fedcba9876543210", "bx_guard", "box", "completed")}, wantErr: true},
		{name: "malformed poll", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), polls: []commandOutcome{{result: core.LocalCommandResult{Stdout: `{"operation":null}`}}}, wantErr: true},
		{name: "changed target", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), polls: []commandOutcome{deletionOutcome(testDeletionID, "bx_other", "box", "completed")}, wantErr: true},
		{name: "changed kind", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), polls: []commandOutcome{deletionOutcome(testDeletionID, "bx_guard", "account", "completed")}, wantErr: true},
		{name: "operation lookup failure", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), polls: []commandOutcome{{result: core.LocalCommandResult{Stderr: "operation not found (404)"}, err: errors.New("exit status 1")}}, wantErr: true},
		{name: "canceled acceptance", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), cancelOn: "delete", wantErr: true},
		{name: "canceled completed response", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), polls: []commandOutcome{deletionOutcome(testDeletionID, "bx_guard", "box", "completed")}, cancelOn: "deletion", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := &releaseCommandRunner{configPath: filepath.Join(t.TempDir(), "config.json"), outcomes: map[string][]commandOutcome{
				"stop": {{result: core.LocalCommandResult{}}}, "delete": {test.initial}, "deletion": append([]commandOutcome(nil), test.polls...),
			}, onAction: func(action string) {
				if action == test.cancelOn {
					cancel()
				}
			}}
			c := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: t.TempDir(), runner: runner, releasePollInterval: time.Nanosecond}
			err := c.ReleaseBox(ctx, "bx_guard", func(context.Context) error { return nil })
			if (err != nil) != test.wantErr {
				t.Fatalf("ReleaseBox error=%v wantErr=%t", err, test.wantErr)
			}
			if test.cancelOn != "" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if !test.wantErr && len(runner.outcomes["deletion"]) != 0 {
				t.Fatalf("returned before observing completed operation: commands=%v", runner.commands)
			}
			for _, command := range runner.commands {
				if strings.Contains(command, " deletion ") && command != "box --no-update --json --org personal --api-url https://ascii.dev deletion status "+testDeletionID {
					t.Fatalf("polled a different operation: %s", command)
				}
			}
		})
	}
}

func TestReleaseBoxPendingOperationHonorsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := &releaseCommandRunner{configPath: filepath.Join(t.TempDir(), "config.json"), outcomes: map[string][]commandOutcome{
			"stop": {{result: core.LocalCommandResult{}}}, "delete": {deletionOutcome(testDeletionID, "bx_guard", "box", "blocked")},
		}}
		c := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: t.TempDir(), runner: runner, releasePollInterval: time.Hour}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		err := c.ReleaseBox(ctx, "bx_guard", func(context.Context) error { return nil })
		if !errors.Is(err, context.DeadlineExceeded) || !containsCommand(runner.commands, "box --no-update --json --org personal --api-url https://ascii.dev delete bx_guard --yes") {
			t.Fatalf("pending deletion err=%v commands=%v", err, runner.commands)
		}
	})
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

	client, err := newAPI(cfg, core.Runtime{Exec: runner})
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
	got, err := newAPI(cfg, core.Runtime{Exec: &fakeCommandRunner{}})
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
		infoResponses: []string{`{"box":{"id":"bx_2","state":"ready","ip":"203.0.113.20","sshEndpoint":"198.51.100.20:19036","expiresAt":"2026-06-10T12:00:00Z"}}`},
	}
	client := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: home, runner: runner}
	box, err := client.CreateBox(context.Background(), createRequest{TTL: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if box.ID != "bx_2" || boxHost(box) != "203.0.113.20" {
		t.Fatalf("box=%#v", box)
	}
	if box.SSHEndpoint != "198.51.100.20:19036" {
		t.Fatalf("ssh endpoint=%q", box.SSHEndpoint)
	}
	if got := boxExpiresAt(box); got != "2026-06-10T12:00:00Z" {
		t.Fatalf("boxExpiresAt=%q, want info response expiration", got)
	}
	if !containsCommand(runner.commands, "box --no-update --json --org personal --api-url https://ascii.dev info bx_2") {
		t.Fatalf("commands missing info poll: %v", runner.commands)
	}
}

func TestClientPreservesObservedGenerationAfterReadinessFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	home := t.TempDir()
	runner := &fakeCommandRunner{
		configPath: home + "/config.json",
		newStdout:  `{"event":"created","id":"bx_2"}`,
		newErr:     errors.New("exit status 1"),
		infoResponses: []string{
			`{"box":{"id":"bx_2","state":"provisioning","createdAt":"2026-08-30T12:00:00Z"}}`,
			`{"box":{"id":"bx_2","state":"ready","ip":"203.0.113.20","createdAt":"2026-08-30T12:00:01Z"}}`,
		},
	}
	c := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: home, runner: runner}
	box, err := c.CreateBox(context.Background(), createRequest{TTL: 30 * time.Minute})
	if err == nil {
		t.Fatal("readiness identity change succeeded")
	}
	if box.ID != "bx_2" || box.createdID != "bx_2" || boxCreationTime(box) != "2026-08-30T12:00:00Z" {
		t.Errorf("creation failure lost its original observed generation: %+v", box)
	}
	replacement := &fakeAPI{box: boxData{ID: "bx_2", CreatedAt: "2026-08-30T12:00:01Z", State: "ready", IP: "203.0.113.20"}}
	b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	if err := b.rollbackBox(context.Background(), replacement, "cbx_123456789abc", box, core.LeaseClaim{}, false); err == nil || replacement.deleted {
		t.Fatalf("unpublished rollback adopted a later generation: err=%v deleted=%t", err, replacement.deleted)
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

// The Boat rename issues "boat_"-prefixed API keys. They must redact like the
// legacy "box_" keys instead of reaching diagnostics in the clear.
func TestRedactBoxSecretsCoversBothKeyPrefixes(t *testing.T) {
	for _, tt := range []struct {
		name, secret, wantPrefix string
	}{
		{"legacy box key", "box_realTokenValue", "box_REDACTED"},
		{"renamed boat key", "boat_realTokenValue", "boat_REDACTED"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := redactBoxSecrets("ascii-box CLI limits failed: rejected " + tt.secret)
			if strings.Contains(got, tt.secret) {
				t.Fatalf("redacted output leaked the key: %q", got)
			}
			if !strings.Contains(got, tt.wantPrefix) {
				t.Fatalf("redacted=%q want it to contain %q", got, tt.wantPrefix)
			}
		})
	}
}

// Pin connection resolution: an advertised endpoint wins, and the ip:22 fallback stays
// available for a box that reports no endpoint at all.
func TestBoxSSHConnectionPrefersAdvertisedEndpoint(t *testing.T) {
	for _, tt := range []struct {
		name     string
		box      boxData
		wantHost string
		wantPort string
		wantErr  bool
	}{
		{name: "advertised endpoint", box: boxData{ID: "bx_1", IP: "203.0.113.10", SSHEndpoint: "198.51.100.20:19040"}, wantHost: "198.51.100.20", wantPort: "19040"},
		{name: "snake_case endpoint alias", box: boxData{ID: "bx_1", SSHEndpointAlt: "198.51.100.20:19041"}, wantHost: "198.51.100.20", wantPort: "19041"},
		{name: "no endpoint falls back to ip:22", box: boxData{ID: "bx_1", IP: "203.0.113.10"}, wantHost: "203.0.113.10", wantPort: "22"},
		{name: "malformed endpoint is rejected", box: boxData{ID: "bx_1", IP: "203.0.113.10", SSHEndpoint: "198.51.100.20"}, wantErr: true},
		{name: "no host at all is rejected", box: boxData{ID: "bx_1"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host, port, err := boxSSHConnection(tt.box)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("host=%q port=%q want error", host, port)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if host != tt.wantHost || port != tt.wantPort {
				t.Fatalf("host=%q port=%q want %q %q", host, port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

// Releasing a lease must survive the rename: the current CLI reports deletion
// operations with kind "sandbox" while older Box CLIs reported "box". Any other
// kind must still be rejected so the claim is retained.
func TestValidateBoxDeletionOperationAcceptsRenamedKind(t *testing.T) {
	const opID = "bdop_e896e624d8af4d9e92cab7848ecb8a83"
	for _, tt := range []struct {
		name, kind string
		wantErr    bool
	}{
		{"renamed sandbox kind", "sandbox", false},
		{"legacy box kind", "box", false},
		{"unrelated kind is rejected", "snapshot", true},
		{"empty kind is rejected", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			operation := boxDeletionOperation{ID: opID, Kind: tt.kind, TargetID: "bx_1", Status: "pending"}
			err := validateBoxDeletionOperation(operation, "bx_1", opID)
			if tt.wantErr && err == nil {
				t.Fatalf("kind %q was accepted", tt.kind)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("kind %q rejected: %v", tt.kind, err)
			}
		})
	}
}

// The CLI owns this key. Crabbox falls forward to the renamed name only when
// the legacy key is absent, so a pre-rename home keeps presenting the exact
// credential it always did and no existing setup can change behavior. The
// legacy key wins whenever it exists, whatever its age relative to a renamed
// key that the configured CLI never authorized.
func TestBoxSSHKeyPrefersTheLegacyKeyWhenPresent(t *testing.T) {
	legacy, renamed := "ascii_box_ed25519", "ascii_sandbox_ed25519"
	for _, tt := range []struct {
		name        string
		present     []string
		legacyOlder bool
		want        string
	}{
		{name: "only the renamed CLI has run", present: []string{renamed}, want: renamed},
		{name: "only a legacy CLI has run", present: []string{legacy}, want: legacy},
		{name: "both present keeps the legacy key", present: []string{legacy, renamed}, want: legacy},
		{name: "legacy key older than an unused renamed key still wins", present: []string{legacy, renamed}, legacyOlder: true, want: legacy},
		{name: "neither present keeps the legacy name", want: legacy},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CRABBOX_ASCII_BOX_HOME", home)
			dir := filepath.Join(home, ".ssh")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, key := range tt.present {
				if err := os.WriteFile(filepath.Join(dir, key), []byte("key"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.legacyOlder {
				old := time.Now().Add(-90 * 24 * time.Hour)
				if err := os.Chtimes(filepath.Join(dir, legacy), old, old); err != nil {
					t.Fatal(err)
				}
			}
			if got, want := boxSSHKey(core.Config{}), filepath.Join(dir, tt.want); got != want {
				t.Fatalf("boxSSHKey()=%q want %q", got, want)
			}
		})
	}
}

// ASCII renamed the Box CLI to Boat. A current install ships only "boat", but an
// explicitly configured command must always be honored exactly as given.
func TestResolveAsciiBoxCLIPrefersInstalledBinary(t *testing.T) {
	for _, tt := range []struct {
		name       string
		configured string
		installed  map[string]bool
		want       string
	}{
		{"legacy box still installed", "box", map[string]bool{"box": true, "boat": true}, "box"},
		{"only renamed boat installed", "box", map[string]bool{"boat": true}, "boat"},
		{"empty falls back to boat when box absent", "", map[string]bool{"boat": true}, "boat"},
		{"neither installed keeps legacy name", "box", nil, "box"},
		{"explicit boat honored", "boat", map[string]bool{"box": true}, "boat"},
		{"explicit absolute path honored", "/opt/ascii/bin/box", nil, "/opt/ascii/bin/box"},
		{"explicit relative path honored", "./box", map[string]bool{"boat": true}, "./box"},
		{"explicit other name honored", "box-legacy", map[string]bool{"boat": true}, "box-legacy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := asciiBoxCLILookPath
			t.Cleanup(func() { asciiBoxCLILookPath = original })
			asciiBoxCLILookPath = func(name string) (string, error) {
				if tt.installed[name] {
					return "/usr/local/bin/" + name, nil
				}
				return "", fmt.Errorf("%s: not found", name)
			}
			if got := resolveAsciiBoxCLI(tt.configured); got != tt.want {
				t.Fatalf("resolveAsciiBoxCLI(%q)=%q want %q", tt.configured, got, tt.want)
			}
		})
	}
}

// ASCII renamed Box to Boat and renamed the CLI's JSON envelope from
// "box"/"boxes" to "sandbox"/"sandboxes". One build must read both.
func TestDecodeBoxAcceptsRenamedEnvelope(t *testing.T) {
	for _, tt := range []struct {
		name, payload string
	}{
		{"renamed sandbox envelope", `{"sandbox":{"id":"bx_1","state":"idle","sshEndpoint":"198.51.100.20:19035"}}`},
		{"legacy box envelope", `{"box":{"id":"bx_1","state":"idle","sshEndpoint":"198.51.100.20:19035"}}`},
		{"bare object", `{"id":"bx_1","state":"idle","sshEndpoint":"198.51.100.20:19035"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			box, err := decodeBox([]byte(tt.payload))
			if err != nil {
				t.Fatal(err)
			}
			if box.ID != "bx_1" || box.State != "idle" || box.SSHEndpoint != "198.51.100.20:19035" {
				t.Fatalf("decoded=%#v", box)
			}
		})
	}
}

func TestDecodeBoxesAcceptsRenamedEnvelope(t *testing.T) {
	for _, tt := range []struct {
		name, payload string
		want          int
	}{
		{"renamed sandboxes envelope", `{"sandboxes":[{"id":"bx_1"},{"id":"bx_2"}],"pageInfo":{"hasMore":false,"nextCursor":null}}`, 2},
		{"legacy boxes envelope", `{"boxes":[{"id":"bx_1"},{"id":"bx_2"}]}`, 2},
		{"renamed empty inventory proves absence", `{"sandboxes":[],"pageInfo":{"hasMore":false,"nextCursor":null}}`, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			boxes, err := decodeBoxes([]byte(tt.payload), true)
			if err != nil {
				t.Fatal(err)
			}
			if len(boxes) != tt.want {
				t.Fatalf("decoded %d boxes, want %d: %#v", len(boxes), tt.want, boxes)
			}
		})
	}
}

// A transitional CLI can report a Box under only one envelope. Cleanup reads
// this inventory as proof that a Box is really gone, so every reported resource
// has to survive decoding or a still-present Box could have its claim removed.
func TestDecodeBoxesReconcilesBothEnvelopes(t *testing.T) {
	for _, tt := range []struct {
		name, payload string
		want          []string
	}{
		{"conflicting equal-length envelopes keep both", `{"sandboxes":[{"id":"bx_other"}],"boxes":[{"id":"bx_target"}]}`, []string{"bx_other", "bx_target"}},
		{"empty sandboxes cannot hide populated boxes", `{"sandboxes":[],"boxes":[{"id":"bx_target"}]}`, []string{"bx_target"}},
		{"empty boxes cannot hide populated sandboxes", `{"boxes":[],"sandboxes":[{"id":"bx_target"}]}`, []string{"bx_target"}},
		{"same Box in both envelopes is not duplicated", `{"sandboxes":[{"id":"bx_target"}],"boxes":[{"id":"bx_target"}]}`, []string{"bx_target"}},
		{"both genuinely empty still proves absence", `{"sandboxes":[],"boxes":[]}`, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			boxes, err := decodeBoxes([]byte(tt.payload), true)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, box := range boxes {
				got = append(got, box.ID)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("decoded %v want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("decoded %v want %v", got, tt.want)
				}
			}
		})
	}
}

// Merging the two envelopes must not lose fields reported by only one of them.
func TestDecodeBoxesMergesFieldsAcrossEnvelopes(t *testing.T) {
	boxes, err := decodeBoxes([]byte(`{"sandboxes":[{"id":"bx_1","state":"ready"}],"boxes":[{"id":"bx_1","sshEndpoint":"198.51.100.20:19036"}]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(boxes) != 1 || boxes[0].State != "ready" || boxes[0].SSHEndpoint != "198.51.100.20:19036" {
		t.Fatalf("merged=%#v", boxes)
	}
}

// A paginated inventory can never prove complete absence, whichever envelope
// the CLI used to report it.
func TestDecodeBoxesRejectsPaginatedRenamedInventory(t *testing.T) {
	if _, err := decodeBoxes([]byte(`{"sandboxes":[{"id":"bx_1"}],"pageInfo":{"hasMore":true,"nextCursor":"c1"}}`), true); err == nil {
		t.Fatal("paginated renamed inventory was accepted as complete")
	}
}

func TestAcquireClaimsBoxAndReturnsSSHTarget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{box: testBox()}
	withFakeAPI(t, fake)
	stubSSHWait(t)

	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{
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
	if claim.Provider != providerName || claim.ProviderScope != (Provider{}).ClaimScope(testConfig()) || claim.Slug != "proof" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestAcquireUsesBoxSSHEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{box: testBox()}
	fake.box.SSHEndpoint = "198.51.100.20:19036"
	withFakeAPI(t, fake)
	stubSSHWait(t)

	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	lease, err := backend.Acquire(context.Background(), core.AcquireRequest{
		Repo: core.Repo{Name: "repo", Root: t.TempDir()},
		Keep: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "198.51.100.20" || lease.SSH.Port != "19036" {
		t.Fatalf("lease SSH=%#v", lease.SSH)
	}
	if lease.Server.PublicNet.IPv4.IP != "203.0.113.10" {
		t.Fatalf("public IP=%q, want provider box IP", lease.Server.PublicNet.IPv4.IP)
	}
}

func TestLeaseFromBoxScopesHostTrustToLease(t *testing.T) {
	for _, endpoint := range []string{"", "198.51.100.20:19036"} {
		t.Run(core.Blank(endpoint, "legacy-ip"), func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			t.Setenv("CRABBOX_ASCII_BOX_HOME", t.TempDir())
			cfg := testConfig()
			sharedKey := boxSSHKey(cfg)
			if err := os.MkdirAll(filepath.Dir(sharedKey), 0o700); err != nil {
				t.Fatal(err)
			}
			sharedTrust := filepath.Join(filepath.Dir(sharedKey), "known_hosts")
			if err := os.WriteFile(sharedTrust, []byte("shared provider trust\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			stubSSHWait(t)
			var waited []core.SSHTarget
			waitForSSHReadyFunc = func(_ context.Context, target *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
				waited = append(waited, *target)
				return nil
			}
			b := NewBackend(Provider{}.Spec(), cfg, testRuntime()).(*backend)
			box := testBox()
			box.SSHEndpoint = endpoint
			first, err := b.leaseFromBox(context.Background(), cfg, box, "cbx_123456789abc", "first", true, true)
			if err != nil {
				t.Fatal(err)
			}
			if first.SSH.KnownHostsFile == "" || first.SSH.KnownHostsFile == sharedTrust || filepath.Base(filepath.Dir(first.SSH.KnownHostsFile)) != first.LeaseID {
				t.Fatalf("host trust is not lease-scoped: %+v", first.SSH)
			}
			const enrolled = "first lease host key\n"
			if err := os.WriteFile(first.SSH.KnownHostsFile, []byte(enrolled), 0o600); err != nil {
				t.Fatal(err)
			}
			again, err := b.leaseFromBox(context.Background(), cfg, box, first.LeaseID, "first", true, true)
			if err != nil {
				t.Fatal(err)
			}
			box.ID = "bx_2"
			second, err := b.leaseFromBox(context.Background(), cfg, box, "cbx_abcdef123456", "second", true, true)
			if err != nil {
				t.Fatal(err)
			}
			if again.SSH.KnownHostsFile != first.SSH.KnownHostsFile || second.SSH.KnownHostsFile == first.SSH.KnownHostsFile || second.SSH.Host != first.SSH.Host || second.SSH.Port != first.SSH.Port {
				t.Fatalf("incorrect host trust reuse: first=%+v again=%+v second=%+v", first.SSH, again.SSH, second.SSH)
			}
			for _, lease := range []core.LeaseTarget{first, again, second} {
				if lease.SSH.Key != sharedKey || lease.SSH.DisableHostKeyChecking || !lease.SSH.NoControlMaster {
					t.Fatalf("changed native authentication or SSH policy: %+v", lease.SSH)
				}
			}
			if len(waited) != 3 || waited[0].KnownHostsFile != first.SSH.KnownHostsFile || waited[2].KnownHostsFile != second.SSH.KnownHostsFile {
				t.Fatalf("readiness did not use scoped trust: %+v", waited)
			}
			if data, err := os.ReadFile(first.SSH.KnownHostsFile); err != nil || string(data) != enrolled {
				t.Fatalf("reuse overwrote enrolled host trust: %q, %v", data, err)
			}
			if _, err := os.Stat(second.SSH.KnownHostsFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("new lease inherited host trust: %v", err)
			}
			if data, err := os.ReadFile(sharedTrust); err != nil || string(data) != "shared provider trust\n" {
				t.Fatalf("shared trust changed: %q, %v", data, err)
			}
		})
	}
}

func TestLeaseFromBoxRejectsUnsafeHostTrustPathBeforeReadiness(t *testing.T) {
	testutil.IsolateUserDirs(t)
	stubSSHWait(t)
	waited := false
	waitForSSHReadyFunc = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		waited = true
		return nil
	}
	b := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	_, err := b.leaseFromBox(context.Background(), testConfig(), testBox(), "../outside", "unsafe", true, true)
	if err == nil || waited {
		t.Fatalf("unsafe lease host trust reached readiness: err=%v waited=%t", err, waited)
	}
}

func TestBoxSSHTargetRejectsMalformedEndpoint(t *testing.T) {
	_, err := boxSSHTarget(testConfig(), boxData{
		ID:          "bx_malformed",
		IP:          "203.0.113.10",
		SSHEndpoint: "gateway-without-port",
	}, "cbx_123456789abc")
	if err == nil || !strings.Contains(err.Error(), "invalid SSH endpoint") {
		t.Fatalf("err=%v, want malformed endpoint error", err)
	}
}

func TestAcquireReleasesPartiallyCreatedBox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{
		box:       boxData{ID: "bx_partial", createdID: "bx_partial", CreatedAt: "2026-08-30T12:00:00Z"},
		createErr: fmt.Errorf("create failed"),
	}
	withFakeAPI(t, fake)

	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	_, err := backend.Acquire(context.Background(), core.AcquireRequest{
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
	if _, err := publishBoxClaim(testConfig(), "cbx_123456789abc", "proof", t.TempDir(), fake.box, true); err != nil {
		t.Fatal(err)
	}

	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "proof"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_123456789abc" || lease.Server.CloudID != "bx_1" || lease.SSH.Host != "203.0.113.10" {
		t.Fatalf("lease=%#v", lease)
	}
	if err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
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
	fake := &fakeAPI{box: boxData{ID: "bx_booting", Status: "provisioning", CreatedAt: "2026-08-30T12:00:00Z"}}
	withFakeAPI(t, fake)
	if _, err := publishBoxClaim(testConfig(), "cbx_abcdef123456", "booting", t.TempDir(), fake.box, true); err != nil {
		t.Fatal(err)
	}

	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	lease, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "booting", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "" || lease.Server.CloudID != "bx_booting" {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestResolveRawBoxIDDoesNotAdopt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{box: boxData{ID: "bx_external", State: "ready", IP: "203.0.113.30"}}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	for _, releaseOnly := range []bool{false, true} {
		_, err := backend.Resolve(context.Background(), core.ResolveRequest{ID: "bx_external", Repo: core.Repo{Root: t.TempDir()}, ReleaseOnly: releaseOnly, Reclaim: true})
		if err == nil {
			t.Fatal("unclaimed raw ID was accepted")
		}
	}
	claims, err := core.ListLeaseClaims()
	if err != nil || len(claims) != 0 || len(fake.prepareIDs) != 0 || len(fake.deletedIDs) != 0 {
		t.Fatalf("unclaimed reuse mutated state: claims=%d err=%v", len(claims), err)
	}
}

func TestStatusMapsBoxAPIFields(t *testing.T) {
	fake := &fakeAPI{box: testBox()}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	view, err := backend.Status(context.Background(), core.StatusRequest{ID: "bx_1"})
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != "ascii_bx_1" || view.ServerID != "bx_1" || view.SSHHost != "203.0.113.10" || view.SSHUser != "user" || !view.Ready {
		t.Fatalf("view=%#v", view)
	}
}

func TestStatusMapsBoxSSHEndpoint(t *testing.T) {
	fake := &fakeAPI{box: testBox()}
	fake.box.SSHEndpoint = "198.51.100.20:19036"
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	view, err := backend.Status(context.Background(), core.StatusRequest{ID: "bx_1"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Host != "203.0.113.10" || view.SSHHost != "198.51.100.20" || view.SSHPort != "19036" {
		t.Fatalf("view=%#v", view)
	}
}

func TestStatusWaitReturnsTerminalBoxState(t *testing.T) {
	fake := &fakeAPI{box: boxData{ID: "bx_failed", State: "error", IP: "203.0.113.10"}}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	view, err := backend.Status(context.Background(), core.StatusRequest{ID: "bx_failed", Wait: true, WaitTimeout: time.Minute})
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

func testConfig() core.Config {
	return core.Config{
		Provider: providerName,
		SSHKey:   "/tmp/global-crabbox-key",
		AsciiBox: core.AsciiBoxConfig{
			APIKey:  "box_key",
			BaseURL: "https://ascii.dev",
			CLIPath: "box",
			Workdir: "/home/user/crabbox",
		},
	}
}

func testRuntime() core.Runtime {
	return core.Runtime{Stdout: io.Discard, Stderr: io.Discard}
}

func testBox() boxData {
	return boxData{ID: "bx_1", createdID: "bx_1", CreatedAt: "2026-08-30T12:00:00Z", State: "ready", IP: "203.0.113.10"}
}

func withFakeAPI(t *testing.T, fake api) {
	t.Helper()
	original := newAPI
	newAPI = func(core.Config, core.Runtime) (api, error) { return fake, nil }
	t.Cleanup(func() { newAPI = original })
}

func stubSSHWait(t *testing.T) {
	t.Helper()
	original := waitForSSHReadyFunc
	waitForSSHReadyFunc = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error { return nil }
	t.Cleanup(func() { waitForSSHReadyFunc = original })
}

type fakeAPI struct {
	createReq    createRequest
	createErr    error
	box          boxData
	prepareIDs   []string
	deletedIDs   []string
	deleted      bool
	getHook      func(string) (boxData, error)
	prepareHook  func(string) error
	listHook     func() ([]boxData, error)
	releaseHook  func(string) error
	deletionHook func(string, string) (boxDeletionOperation, error)
}

func (f *fakeAPI) CreateBox(_ context.Context, req createRequest) (boxData, error) {
	f.createReq = req
	if f.createErr != nil {
		return f.box, f.createErr
	}
	if f.box.ID == "" {
		f.box = testBox()
	}
	return f.box, nil
}

func (f *fakeAPI) Check(context.Context) error { return nil }

func (f *fakeAPI) PrepareSSH(_ context.Context, id string) error {
	f.prepareIDs = append(f.prepareIDs, id)
	if f.prepareHook != nil {
		return f.prepareHook(id)
	}
	return nil
}

func (f *fakeAPI) GetBox(_ context.Context, id string) (boxData, error) {
	if f.getHook != nil {
		return f.getHook(id)
	}
	if f.deleted {
		return boxData{}, fmt.Errorf("404 not found")
	}
	if f.box.ID == "" {
		f.box = testBox()
	}
	if id != f.box.ID {
		return boxData{}, fmt.Errorf("404 not found")
	}
	return f.box, nil
}

func (f *fakeAPI) ListBoxes(context.Context, bool) ([]boxData, error) {
	if f.listHook != nil {
		return f.listHook()
	}
	if f.deleted {
		return []boxData{}, nil
	}
	if f.box.ID == "" {
		f.box = testBox()
	}
	return []boxData{f.box}, nil
}

func (f *fakeAPI) ReleaseBox(ctx context.Context, id string, validate func(context.Context) error) error {
	if err := validate(ctx); err != nil {
		return err
	}
	if f.releaseHook != nil {
		if err := f.releaseHook(id); err != nil {
			return err
		}
	}
	f.deletedIDs = append(f.deletedIDs, id)
	f.deleted = true
	return nil
}

func (f *fakeAPI) GetDeletionOperation(_ context.Context, targetID, operationID string) (boxDeletionOperation, error) {
	if f.deletionHook != nil {
		return f.deletionHook(targetID, operationID)
	}
	return boxDeletionOperation{}, errors.New("unexpected deletion operation lookup")
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
	result core.LocalCommandResult
	err    error
}

type releaseCommandRunner struct {
	commands   []string
	configPath string
	outcomes   map[string][]commandOutcome
	onAction   func(string)
}

func (r *releaseCommandRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.commands = append(r.commands, strings.Join(append([]string{req.Name}, req.Args...), " "))
	action := boxCLIAction(req.Args)
	if action == "status" {
		return core.LocalCommandResult{Stdout: fmt.Sprintf(`{"account":null,"api":{},"config":{"path":%q}}`, r.configPath)}, nil
	}
	queue := r.outcomes[action]
	if len(queue) == 0 {
		return core.LocalCommandResult{Stderr: "unexpected command"}, fmt.Errorf("unexpected %s command", action)
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
		case "status", "stop", "delete", "deletion", "extend", "info", "list":
			return arg
		}
	}
	return ""
}

func snapshotGuardOutcome() commandOutcome {
	return commandOutcome{
		result: core.LocalCommandResult{Stdout: `{"code":"snapshot_required","error":"Refusing request: no successful snapshot in the last 30 minutes.","status":409}`},
		err:    fmt.Errorf("exit status 1"),
	}
}

func (r *fakeCommandRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.commands = append(r.commands, strings.Join(append([]string{req.Name}, req.Args...), " "))
	r.env = append(r.env, req.Env)
	joined := strings.Join(req.Args, " ")
	switch {
	case strings.Contains(joined, " deletion status "+testDeletionID):
		return deletionOutcome(testDeletionID, "bx_1", "box", "completed").result, nil
	case strings.Contains(joined, " status"):
		return core.LocalCommandResult{Stdout: fmt.Sprintf(`{"account":null,"api":{},"config":{"path":%q}}`, r.configPath)}, nil
	case strings.Contains(joined, " new "):
		if r.newStdout != "" || r.newErr != nil {
			return core.LocalCommandResult{Stdout: r.newStdout}, r.newErr
		}
		return core.LocalCommandResult{Stdout: strings.Join([]string{
			`{"event":"created","id":"bx_1","ttlSeconds":1800}`,
			`{"event":"state","id":"bx_1","state":"provisioning"}`,
			`{"event":"ready","id":"bx_1","state":"ready","ip":"203.0.113.10","archiveAfter":"2026-05-30T20:00:00Z"}`,
		}, "\n")}, nil
	case strings.Contains(joined, " ssh bx_1 -- true"):
		return core.LocalCommandResult{}, nil
	case strings.Contains(joined, " info bx_1"):
		return core.LocalCommandResult{Stdout: `{"box":{"id":"bx_1","state":"ready","ip":"203.0.113.10"}}`}, nil
	case strings.Contains(joined, " info bx_2"):
		if len(r.infoResponses) == 0 {
			return core.LocalCommandResult{Stderr: "missing info response"}, fmt.Errorf("missing info response")
		}
		out := r.infoResponses[0]
		r.infoResponses = r.infoResponses[1:]
		return core.LocalCommandResult{Stdout: out}, nil
	case strings.Contains(joined, " list"):
		return core.LocalCommandResult{Stdout: `{"boxes":[{"id":"bx_1","state":"ready","ip":"203.0.113.10"}]}`}, nil
	case strings.Contains(joined, " stop bx_1"):
		return core.LocalCommandResult{Stdout: `{"id":"bx_1","status":"deleted"}`}, nil
	case strings.Contains(joined, " delete bx_1"):
		return deletionOutcome(testDeletionID, "bx_1", "box", "completed").result, nil
	default:
		return core.LocalCommandResult{Stderr: "unexpected command"}, fmt.Errorf("unexpected command")
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
