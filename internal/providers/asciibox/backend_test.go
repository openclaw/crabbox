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
	for _, req := range runner.requests {
		if req.MaxCapturedOutputBytes <= 0 || req.MaxCapturedOutputBytes > 8<<20 || req.DisableOutputCapture || req.Stdout != nil || req.Stderr != nil {
			t.Fatalf("native command must use bounded, non-streaming capture: %+v", req.Args)
		}
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
			"extend":   {{result: LocalCommandResult{Stdout: `{"id":"bx_guard","archiveAfter":"soon"}`}}},
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
			"stop":   {{result: LocalCommandResult{}}},
			"delete": {{result: LocalCommandResult{Stderr: "permission denied"}, err: fmt.Errorf("exit status 1")}},
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
			"extend": {{result: LocalCommandResult{Stderr: "extend throttled"}, err: fmt.Errorf("exit status 1")}},
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
	return commandOutcome{result: LocalCommandResult{Stdout: fmt.Sprintf(`{"operation":{"id":%q,"targetId":%q,"kind":%q,"status":%q,"completedAt":%s}}`, id, target, kind, state, completedAt)}}
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
		{name: "missing receipt", initial: commandOutcome{result: LocalCommandResult{Stdout: `{}`}}, wantErr: true},
		{name: "legacy deleted response", initial: commandOutcome{result: LocalCommandResult{Stdout: `{"id":"bx_guard","status":"deleted"}`}}, wantErr: true},
		{name: "malformed receipt", initial: commandOutcome{result: LocalCommandResult{Stdout: `not json`}}, wantErr: true},
		{name: "invalid operation ID", initial: deletionOutcome("bdop_other", "bx_guard", "box", "completed"), wantErr: true},
		{name: "wrong initial target", initial: deletionOutcome(testDeletionID, "bx_other", "box", "completed"), wantErr: true},
		{name: "wrong initial kind", initial: deletionOutcome(testDeletionID, "bx_guard", "account", "completed"), wantErr: true},
		{name: "unknown state", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "deleted"), wantErr: true},
		{name: "missing completion timestamp", initial: commandOutcome{result: LocalCommandResult{Stdout: fmt.Sprintf(`{"operation":{"id":%q,"targetId":"bx_guard","kind":"box","status":"completed","completedAt":null}}`, testDeletionID)}}, wantErr: true},
		{name: "invalid completion timestamp", initial: commandOutcome{result: LocalCommandResult{Stdout: fmt.Sprintf(`{"operation":{"id":%q,"targetId":"bx_guard","kind":"box","status":"completed","completedAt":"yesterday"}}`, testDeletionID)}}, wantErr: true},
		{name: "changed operation", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), polls: []commandOutcome{deletionOutcome("bdop_fedcba9876543210fedcba9876543210", "bx_guard", "box", "completed")}, wantErr: true},
		{name: "malformed poll", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), polls: []commandOutcome{{result: LocalCommandResult{Stdout: `{"operation":null}`}}}, wantErr: true},
		{name: "changed target", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), polls: []commandOutcome{deletionOutcome(testDeletionID, "bx_other", "box", "completed")}, wantErr: true},
		{name: "changed kind", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), polls: []commandOutcome{deletionOutcome(testDeletionID, "bx_guard", "account", "completed")}, wantErr: true},
		{name: "operation lookup failure", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), polls: []commandOutcome{{result: LocalCommandResult{Stderr: "operation not found (404)"}, err: errors.New("exit status 1")}}, wantErr: true},
		{name: "canceled acceptance", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), cancelOn: "delete", wantErr: true},
		{name: "canceled completed response", initial: deletionOutcome(testDeletionID, "bx_guard", "box", "pending"), polls: []commandOutcome{deletionOutcome(testDeletionID, "bx_guard", "box", "completed")}, cancelOn: "deletion", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := &releaseCommandRunner{configPath: filepath.Join(t.TempDir(), "config.json"), outcomes: map[string][]commandOutcome{
				"stop": {{result: LocalCommandResult{}}}, "delete": {test.initial}, "deletion": append([]commandOutcome(nil), test.polls...),
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
	runner := &releaseCommandRunner{configPath: filepath.Join(t.TempDir(), "config.json"), outcomes: map[string][]commandOutcome{
		"stop": {{result: LocalCommandResult{}}}, "delete": {deletionOutcome(testDeletionID, "bx_guard", "box", "blocked")},
	}}
	c := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: t.TempDir(), runner: runner, releasePollInterval: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := c.ReleaseBox(ctx, "bx_guard", func(context.Context) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) || !containsCommand(runner.commands, "box --no-update --json --org personal --api-url https://ascii.dev delete bx_guard --yes") {
		t.Fatalf("pending deletion err=%v commands=%v", err, runner.commands)
	}
}

func TestReleaseBoxReportsLastDeletionStatusWhenNativeLookupTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	lookups := 0
	runner := &releaseCommandRunner{configPath: filepath.Join(t.TempDir(), "config.json"), outcomes: map[string][]commandOutcome{
		"stop":   {{result: LocalCommandResult{}}},
		"delete": {deletionOutcome(testDeletionID, "bx_guard", "box", "pending")},
		"deletion": {
			deletionOutcome(testDeletionID, "bx_guard", "box", "blocked"),
			{err: context.DeadlineExceeded},
		},
	}, onAction: func(action string) {
		if action == "deletion" {
			lookups++
			if lookups == 2 {
				<-ctx.Done()
			}
		}
	}}
	c := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: t.TempDir(), runner: runner, releasePollInterval: time.Nanosecond}
	err := c.ReleaseBox(ctx, "bx_guard", func(context.Context) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost deadline cause: %v", err)
	}
	for _, want := range []string{"phase=deletion-operation", testDeletionID, "last_observed_status=blocked", "retaining claim"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
	var incomplete *boxDeletionIncompleteError
	if !errors.As(err, &incomplete) || incomplete.operation.ID != testDeletionID || incomplete.operation.Status != "pending" {
		t.Fatalf("lost original accepted operation: %v", err)
	}
}

func TestBoxCleanupProgressReportsDuringNativeCallAndJoins(t *testing.T) {
	output := make(boxProgressOutput, 32)
	ctx, cancel := context.WithTimeout(withBoxCleanupProgress(context.Background(), output), time.Second)
	defer cancel()
	ctx.Value(boxCleanupProgressKey{}).(*boxCleanupProgress).interval = 5 * time.Millisecond
	entered := make(chan struct{})
	runner := boxCommandRunnerFunc(func(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		close(entered)
		<-ctx.Done()
		return LocalCommandResult{Stdout: "native output must not become progress"}, ctx.Err()
	})
	c := &client{cliPath: "box", runner: runner}
	done := make(chan error, 1)
	go func() { _, err := c.runPrepared(ctx, "delete", "bx_guard", "--yes"); done <- err }()
	<-entered
	for range 2 {
		select {
		case line := <-output:
			if !strings.Contains(line, "phase=native-delete") || !strings.Contains(line, "remaining=") || strings.Contains(line, "native output") {
				t.Fatalf("unexpected progress: %s", line)
			}
		case <-time.After(time.Second):
			t.Fatal("no progress while native command was blocked")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("native cancellation lost: %v", err)
	}
	for len(output) > 0 {
		<-output
	}
	select {
	case line := <-output:
		t.Fatalf("progress after native command returned: %s", line)
	case <-time.After(15 * time.Millisecond):
	}
}

func TestBoxCleanupProgressRetainsCadenceAcrossFastPolls(t *testing.T) {
	output := make(boxProgressOutput, 32)
	ctx := withBoxCleanupProgress(context.Background(), output)
	ctx.Value(boxCleanupProgressKey{}).(*boxCleanupProgress).interval = 5 * time.Millisecond
	c := &client{cliPath: "box", runner: boxCommandRunnerFunc(func(context.Context, LocalCommandRequest) (LocalCommandResult, error) {
		return LocalCommandResult{}, nil
	})}
	deadline := time.Now().Add(time.Second)
	for len(output) < 2 && time.Now().Before(deadline) {
		if _, err := c.runPrepared(ctx, "deletion", "status", testDeletionID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(output) < 2 {
		t.Fatal("fast native calls reset progress cadence")
	}
	for len(output) > 0 {
		if line := <-output; !strings.Contains(line, "phase=deletion-operation") || strings.Contains(line, "remaining=-") {
			t.Fatalf("unexpected progress: %s", line)
		}
	}
}

func TestNativeCaptureErrorCannotAuthorizeCleanupJSON(t *testing.T) {
	for _, action := range []string{"status", "list", "deletion"} {
		t.Run(action, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.json")
			runner := boxCommandRunnerFunc(func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
				current := boxCLIAction(req.Args)
				result := LocalCommandResult{Stdout: fmt.Sprintf(`{"config":{"path":%q}}`, configPath)}
				if current == "list" {
					result.Stdout = `{"boxes":[],"pageInfo":{"hasMore":false}}`
				} else if current == "deletion" {
					result = deletionOutcome(testDeletionID, "bx_guard", "box", "completed").result
				}
				if current == action {
					return result, errors.New("captured command output exceeded limit")
				}
				return result, nil
			})
			c := &client{apiKey: "box_key", apiURL: "https://ascii.dev", cliPath: "box", home: t.TempDir(), runner: runner}
			var err error
			if action == "deletion" {
				_, err = c.GetDeletionOperation(context.Background(), "bx_guard", testDeletionID)
			} else {
				_, err = c.ListBoxes(context.Background(), true)
			}
			if err == nil {
				t.Fatal("valid-looking JSON from failed capture was accepted")
			}
		})
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
	if err := b.rollbackBox(context.Background(), replacement, "cbx_123456789abc", box, LeaseClaim{}, false); err == nil || replacement.deleted {
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
	lease, err := backend.Acquire(context.Background(), AcquireRequest{
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

func TestBoxSSHTargetRejectsMalformedEndpoint(t *testing.T) {
	_, err := boxSSHTarget(testConfig(), boxData{
		ID:          "bx_malformed",
		IP:          "203.0.113.10",
		SSHEndpoint: "gateway-without-port",
	})
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
	if _, err := publishBoxClaim(testConfig(), "cbx_123456789abc", "proof", t.TempDir(), fake.box, true); err != nil {
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
	fake := &fakeAPI{box: boxData{ID: "bx_booting", Status: "provisioning", CreatedAt: "2026-08-30T12:00:00Z"}}
	withFakeAPI(t, fake)
	if _, err := publishBoxClaim(testConfig(), "cbx_abcdef123456", "booting", t.TempDir(), fake.box, true); err != nil {
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

func TestResolveRawBoxIDDoesNotAdopt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeAPI{box: boxData{ID: "bx_external", State: "ready", IP: "203.0.113.30"}}
	withFakeAPI(t, fake)
	backend := NewBackend(Provider{}.Spec(), testConfig(), testRuntime()).(*backend)
	for _, releaseOnly := range []bool{false, true} {
		_, err := backend.Resolve(context.Background(), ResolveRequest{ID: "bx_external", Repo: core.Repo{Root: t.TempDir()}, ReleaseOnly: releaseOnly, Reclaim: true})
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
	view, err := backend.Status(context.Background(), StatusRequest{ID: "bx_1"})
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
	view, err := backend.Status(context.Background(), StatusRequest{ID: "bx_1"})
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
	return boxData{ID: "bx_1", createdID: "bx_1", CreatedAt: "2026-08-30T12:00:00Z", State: "ready", IP: "203.0.113.10"}
}

func withFakeAPI(t *testing.T, fake api) {
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
	requests   []LocalCommandRequest
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
		case "status", "stop", "delete", "deletion", "extend", "info", "list":
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
	r.requests = append(r.requests, req)
	r.commands = append(r.commands, strings.Join(append([]string{req.Name}, req.Args...), " "))
	r.env = append(r.env, req.Env)
	joined := strings.Join(req.Args, " ")
	switch {
	case strings.Contains(joined, " deletion status "+testDeletionID):
		return deletionOutcome(testDeletionID, "bx_1", "box", "completed").result, nil
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
		return deletionOutcome(testDeletionID, "bx_1", "box", "completed").result, nil
	default:
		return LocalCommandResult{Stderr: "unexpected command"}, fmt.Errorf("unexpected command")
	}
}

type boxCommandRunnerFunc func(context.Context, LocalCommandRequest) (LocalCommandResult, error)

func (f boxCommandRunnerFunc) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	return f(ctx, req)
}

type boxProgressOutput chan string

func (out boxProgressOutput) Write(data []byte) (int, error) {
	out <- string(data)
	return len(data), nil
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
