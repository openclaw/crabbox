package cli

import (
	"context"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIsloWarmupArgs(t *testing.T) {
	cfg := baseConfig()
	cfg.Islo = IsloConfig{
		Image:          "docker.io/library/ubuntu:24.04",
		Source:         "github://openclaw/crabbox:main",
		Workdir:        "/workspace/crabbox",
		GatewayProfile: "default",
		Session:        "main",
	}
	got := isloWarmupArgs(cfg, "crabbox-deadbeef")
	want := []string{
		"use", "crabbox-deadbeef",
		"--image", "docker.io/library/ubuntu:24.04",
		"--source", "github://openclaw/crabbox:main",
		"--workdir", "/workspace/crabbox",
		"--gateway-profile", "default",
		"--session", "main",
		"--", "true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v want %#v", got, want)
	}
}

func TestIsloRunArgsLeadingEnvUsesShell(t *testing.T) {
	cfg := baseConfig()
	cfg.Islo.Image = "docker.io/library/ubuntu:24.04"
	got := isloRunArgs(cfg, "crabbox-abc", []string{"OPENCLAW_TESTBOX=1", "pnpm", "check:changed"}, false)
	want := []string{
		"use", "crabbox-abc",
		"--image", "docker.io/library/ubuntu:24.04",
		"--", "bash", "-lc", "OPENCLAW_TESTBOX='1' 'pnpm' 'check:changed'",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v want %#v", got, want)
	}
}

func TestIsloRunArgsArgvWithoutEnv(t *testing.T) {
	cfg := baseConfig()
	got := isloRunArgs(cfg, "sb", []string{"go", "test", "./..."}, false)
	want := []string{"use", "sb", "--", "go", "test", "./..."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v want %#v", got, want)
	}
}

func TestIsloCommandStringQuoting(t *testing.T) {
	tests := []struct {
		name    string
		command []string
		want    string
	}{
		{name: "argv", command: []string{"pnpm", "test", "has space"}, want: "'pnpm' 'test' 'has space'"},
		{name: "env assignment", command: []string{"FOO=1", "BAR=value with spaces", "pnpm", "check"}, want: "FOO='1' BAR='value with spaces' 'pnpm' 'check'"},
		{name: "env after command stays argv", command: []string{"echo", "FOO=1"}, want: "'echo' 'FOO=1'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isloCommandString(tt.command); got != tt.want {
				t.Fatalf("got=%q want=%q", got, tt.want)
			}
		})
	}
}

func TestHasLeadingEnvAssignment(t *testing.T) {
	if !hasLeadingEnvAssignment([]string{"FOO=1", "cmd"}) {
		t.Error("FOO=1 cmd should be detected")
	}
	if hasLeadingEnvAssignment([]string{"cmd", "FOO=1"}) {
		t.Error("cmd FOO=1 should NOT be detected (env after cmd)")
	}
	if hasLeadingEnvAssignment([]string{}) {
		t.Error("empty command should NOT be detected")
	}
	if hasLeadingEnvAssignment([]string{"go", "test"}) {
		t.Error("plain argv should NOT be detected")
	}
}

func TestIsloStatusRejectsJSON(t *testing.T) {
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.isloStatus(context.Background(), baseConfig(), "sb", false, 0, true)
	if err == nil || !strings.Contains(err.Error(), "does not support --json") {
		t.Fatalf("expected --json rejection, got %v", err)
	}
}

func TestIsloRunArgsShellMode(t *testing.T) {
	cfg := baseConfig()
	got := isloRunArgs(cfg, "sb", []string{"pnpm", "install", "&&", "pnpm", "test"}, false)
	want := []string{
		"use", "sb",
		"--", "bash", "-lc", "pnpm install && pnpm test",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v want %#v", got, want)
	}
}

func TestIsloRunArgsExplicitShellMode(t *testing.T) {
	cfg := baseConfig()
	got := isloRunArgs(cfg, "sb", []string{"echo", "hello"}, true)
	want := []string{
		"use", "sb",
		"--", "bash", "-lc", "echo hello",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v want %#v", got, want)
	}
}

func TestIsloListArgsJSON(t *testing.T) {
	cfg := baseConfig()
	got := isloListArgs(cfg, true)
	want := []string{"-o", "json", "ls"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v want %#v", got, want)
	}
}

func TestIsloStopArgs(t *testing.T) {
	cfg := baseConfig()
	got := isloStopArgs(cfg, "my-sandbox")
	want := []string{"rm", "my-sandbox", "--force"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v want %#v", got, want)
	}
}

func TestIsloStatusArgs(t *testing.T) {
	cfg := baseConfig()
	if got := isloStatusArgs(cfg, "sb", false); !reflect.DeepEqual(got, []string{"status", "sb"}) {
		t.Fatalf("plain args=%#v", got)
	}
	if got := isloStatusArgs(cfg, "sb", true); !reflect.DeepEqual(got, []string{"-o", "json", "status", "sb"}) {
		t.Fatalf("json args=%#v", got)
	}
}

func TestApplyIsloFlagOverrides(t *testing.T) {
	defaults := baseConfig()
	defaults.Islo = IsloConfig{
		Org:   "default-org",
		Image: "default-image",
	}
	cfg := Config{}
	fs := newFlagSet("test", io.Discard)
	values := registerIsloFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--islo-org", "openclaw",
		"--islo-image", "docker.io/library/ubuntu:24.04",
		"--islo-source", "github://openclaw/crabbox",
		"--islo-workdir", "/workspace",
		"--islo-gateway-profile", "default",
		"--islo-session", "main",
	}); err != nil {
		t.Fatal(err)
	}
	applyIsloFlagOverrides(&cfg, fs, values)
	want := IsloConfig{
		Org:            "openclaw",
		Image:          "docker.io/library/ubuntu:24.04",
		Source:         "github://openclaw/crabbox",
		Workdir:        "/workspace",
		GatewayProfile: "default",
		Session:        "main",
	}
	if !reflect.DeepEqual(cfg.Islo, want) {
		t.Fatalf("islo flags not applied: got=%#v want=%#v", cfg.Islo, want)
	}
}

func TestParseIsloListJSONArray(t *testing.T) {
	got := parseIsloListJSON([]byte(`[
    {"id":"019deeda-e2f2-70c7-b884-5dbe218b8e07","name":"crabbox-blue","status":"running","image":"ubuntu","created_by":"me@example.test","created_at":"2026-05-01T00:00:00Z","deleted_at":null},
    {"id":"sbx_02","name":"alpha","status":"stopped"}
  ]`))
	if len(got) != 2 {
		t.Fatalf("items=%d want 2", len(got))
	}
	if got[0].ID != "019deeda-e2f2-70c7-b884-5dbe218b8e07" || got[0].Name != "crabbox-blue" || got[0].Status != "running" || got[0].CreatedBy != "me@example.test" {
		t.Fatalf("unexpected item 0: %#v", got[0])
	}
	if got[1].Status != "stopped" {
		t.Fatalf("unexpected item 1: %#v", got[1])
	}
}

func TestParseIsloListJSONEmpty(t *testing.T) {
	if got := parseIsloListJSON([]byte("")); len(got) != 0 {
		t.Fatalf("items=%d want 0", len(got))
	}
	if got := parseIsloListJSON([]byte("   \n")); len(got) != 0 {
		t.Fatalf("items=%d want 0", len(got))
	}
	if got := parseIsloListJSON([]byte("[]")); len(got) != 0 {
		t.Fatalf("items=%d want 0", len(got))
	}
	if got := parseIsloListJSON([]byte("not-json")); got == nil {
		t.Fatal("items=nil want empty slice for JSON []")
	}
}

func TestParseIsloListJSONWrapped(t *testing.T) {
	got := parseIsloListJSON([]byte(`{"sandboxes":[{"id":"a","name":"x","status":"running"}]}`))
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("unexpected: %#v", got)
	}
}

func TestIsIsloProvider(t *testing.T) {
	if !isIsloProvider("islo") {
		t.Error("islo not recognized")
	}
	if !isIsloProvider("islo-sandbox") {
		t.Error("islo-sandbox alias not recognized")
	}
	if isIsloProvider("hetzner") {
		t.Error("hetzner falsely matched")
	}
	if isIsloProvider("blacksmith-testbox") {
		t.Error("blacksmith falsely matched")
	}
}

func TestResolveIsloLeaseID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if name, lease, err := resolveIsloLeaseID("isb_my-sandbox", "/repo", false); err != nil || name != "my-sandbox" || lease != "isb_my-sandbox" {
		t.Fatalf("prefixed id name=%q lease=%q err=%v", name, lease, err)
	}
	if name, lease, err := resolveIsloLeaseID("plain-name", "/repo", false); err != nil || name != "plain-name" || lease != "isb_plain-name" {
		t.Fatalf("plain name fallback name=%q lease=%q err=%v", name, lease, err)
	}
	if err := claimLeaseForRepoProvider("isb_crabbox-abc", "Blue Lobster", isloProvider, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	name, lease, err := resolveIsloLeaseID("blue-lobster", "/repo", false)
	if err != nil {
		t.Fatal(err)
	}
	if lease != "isb_crabbox-abc" || name != "crabbox-abc" {
		t.Fatalf("slug resolution: name=%q lease=%q", name, lease)
	}
	if _, _, err := resolveIsloLeaseID("blue-lobster", "/other", false); err == nil || !strings.Contains(err.Error(), "use --reclaim") {
		t.Fatalf("expected repo claim error, got %v", err)
	}
	if name, lease, err := resolveIsloLeaseID("blue-lobster", "/other", true); err != nil || name != "crabbox-abc" || lease != "isb_crabbox-abc" {
		t.Fatalf("reclaim path name=%q lease=%q err=%v", name, lease, err)
	}
}

func TestIsloRejectsSyncOnly(t *testing.T) {
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.isloRun(context.Background(), baseConfig(), Repo{Root: "/repo"}, isloRunOptions{SyncOnly: true})
	if err == nil || !strings.Contains(err.Error(), "--sync-only is not supported") {
		t.Fatalf("expected sync-only rejection, got %v", err)
	}
}

func TestIsloOneShotRunRemovesClaimAfterStop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	t.Setenv("XDG_STATE_HOME", home+"/.local/state")

	original := isloCommandContext
	calls := 0
	var stopArgs []string
	isloCommandContext = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		calls++
		if len(args) >= 1 && args[0] == "rm" {
			stopArgs = append([]string{}, args...)
			return exec.Command("sh", "-c", "exit 0")
		}
		return exec.Command("sh", "-c", "exit 0")
	}
	t.Cleanup(func() { isloCommandContext = original })

	cfg := baseConfig()
	cfg.Islo.Image = "docker.io/library/ubuntu:24.04"
	app := App{Stdout: io.Discard, Stderr: io.Discard}

	if err := app.isloRun(context.Background(), cfg, Repo{Root: "/repo"}, isloRunOptions{Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if calls < 3 {
		t.Fatalf("islo calls=%d, want at least warmup/run/stop", calls)
	}
	if len(stopArgs) == 0 || stopArgs[0] != "rm" || stopArgs[len(stopArgs)-1] != "--force" {
		t.Fatalf("expected rm --force in stop args, got %v", stopArgs)
	}
}
