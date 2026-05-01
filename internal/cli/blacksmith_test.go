package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBlacksmithWarmupArgs(t *testing.T) {
	cfg := baseConfig()
	cfg.Blacksmith = BlacksmithConfig{
		Org:         "openclaw",
		Workflow:    ".github/workflows/testbox.yml",
		Job:         "check",
		Ref:         "main",
		IdleTimeout: 90*time.Minute + 10*time.Second,
	}
	got, err := blacksmithWarmupArgs(cfg, "ssh-ed25519 AAAA")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--org", "openclaw",
		"testbox", "warmup", ".github/workflows/testbox.yml",
		"--job", "check",
		"--ref", "main",
		"--ssh-public-key", "ssh-ed25519 AAAA",
		"--idle-timeout", "91",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v want %#v", got, want)
	}
}

func TestBlacksmithWarmupArgsFallsBackToActionsConfig(t *testing.T) {
	cfg := baseConfig()
	cfg.Actions.Workflow = ".github/workflows/ci.yml"
	cfg.Actions.Job = "hydrate"
	cfg.Actions.Ref = "trunk"
	got, err := blacksmithWarmupArgs(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".github/workflows/ci.yml", "--job", "hydrate", "--ref", "trunk"} {
		if !containsString(got, want) {
			t.Fatalf("args missing %q: %#v", want, got)
		}
	}
}

func TestBlacksmithWarmupArgsRequiresWorkflow(t *testing.T) {
	cfg := baseConfig()
	_, err := blacksmithWarmupArgs(cfg, "")
	if err == nil || !strings.Contains(err.Error(), "requires blacksmith.workflow") {
		t.Fatalf("expected workflow error, got %v", err)
	}
}

func TestBlacksmithWarmupFailureRemovesPendingKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	original := blacksmithCommandContext
	blacksmithCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "exit 1")
	}
	t.Cleanup(func() {
		blacksmithCommandContext = original
	})

	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	_, _, err := app.blacksmithWarmupLease(context.Background(), cfg, Repo{Root: "/repo"}, false)
	if err == nil {
		t.Fatal("expected warmup failure")
	}
	keyPath, keyErr := testboxKeyPath("tbx_probe")
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	entries, readErr := os.ReadDir(filepath.Dir(filepath.Dir(keyPath)))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("pending key directories leaked: %v", entries)
	}
}

func TestBlacksmithOneShotRunRemovesClaimAfterStop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	original := blacksmithCommandContext
	calls := 0
	blacksmithCommandContext = func(_ context.Context, _ string, args ...string) *exec.Cmd {
		calls++
		if len(args) >= 3 && args[0] == "testbox" && args[1] == "warmup" {
			return exec.Command("sh", "-c", "printf 'ready tbx_abc123\\n'")
		}
		return exec.Command("sh", "-c", "exit 0")
	}
	t.Cleanup(func() {
		blacksmithCommandContext = original
	})

	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.blacksmithRun(context.Background(), cfg, Repo{Root: "/repo"}, blacksmithRunOptions{
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("blacksmith calls=%d, want warmup/run/stop", calls)
	}
	if claim, err := readLeaseClaim("tbx_abc123"); err != nil {
		t.Fatal(err)
	} else if claim.LeaseID != "" {
		t.Fatalf("claim leaked after one-shot stop: %#v", claim)
	}
	if keyPath, err := testboxKeyPath("tbx_abc123"); err != nil {
		t.Fatal(err)
	} else if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("key leaked after one-shot stop: %v", err)
	}
}

func TestApplyBlacksmithFlagOverrides(t *testing.T) {
	defaults := baseConfig()
	defaults.Blacksmith = BlacksmithConfig{
		Org:      "default-org",
		Workflow: "default.yml",
		Job:      "default-job",
		Ref:      "main",
	}
	cfg := Config{}
	fs := newFlagSet("test", io.Discard)
	values := registerBlacksmithFlags(fs, defaults)
	if err := parseFlags(fs, []string{
		"--blacksmith-org", "openclaw",
		"--blacksmith-workflow", ".github/workflows/testbox.yml",
		"--blacksmith-job", "test",
		"--blacksmith-ref", "feature",
	}); err != nil {
		t.Fatal(err)
	}
	applyBlacksmithFlagOverrides(&cfg, fs, values)
	if cfg.Blacksmith.Org != "openclaw" || cfg.Blacksmith.Workflow != ".github/workflows/testbox.yml" || cfg.Blacksmith.Job != "test" || cfg.Blacksmith.Ref != "feature" {
		t.Fatalf("blacksmith flags not applied: %#v", cfg.Blacksmith)
	}
}

func TestBlacksmithRunArgs(t *testing.T) {
	cfg := baseConfig()
	cfg.Blacksmith.Org = "openclaw"
	got := blacksmithRunArgs(cfg, "tbx_abc123", "/tmp/key", []string{"OPENCLAW_TESTBOX=1", "pnpm", "check:changed"}, true, false)
	want := []string{
		"--org", "openclaw",
		"testbox", "run",
		"--id", "tbx_abc123",
		"--ssh-private-key", "/tmp/key",
		"--debug",
		"OPENCLAW_TESTBOX='1' 'pnpm' 'check:changed'",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v want %#v", got, want)
	}
}

func TestBlacksmithCommandString(t *testing.T) {
	tests := []struct {
		name      string
		command   []string
		shellMode bool
		want      string
	}{
		{
			name:    "argv",
			command: []string{"pnpm", "test", "has space"},
			want:    "'pnpm' 'test' 'has space'",
		},
		{
			name:    "env assignment",
			command: []string{"OPENCLAW_TESTBOX=1", "NODE_OPTIONS=--max-old-space-size=4096", "pnpm", "check"},
			want:    "OPENCLAW_TESTBOX='1' NODE_OPTIONS='--max-old-space-size=4096' 'pnpm' 'check'",
		},
		{
			name:    "operator uses shell",
			command: []string{"pnpm", "install", "&&", "pnpm", "test"},
			want:    "pnpm install && pnpm test",
		},
		{
			name:      "explicit shell",
			command:   []string{"echo", "hello"},
			shellMode: true,
			want:      "echo hello",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := blacksmithCommandString(tt.command, tt.shellMode); got != tt.want {
				t.Fatalf("command=%q want %q", got, tt.want)
			}
		})
	}
}

func TestParseBlacksmithID(t *testing.T) {
	if got := parseBlacksmithID("ready: tbx_abc-123_more"); got != "tbx_abc-123_more" {
		t.Fatalf("id=%q", got)
	}
	if got := parseBlacksmithID("ready: cbx_abc"); got != "" {
		t.Fatalf("id=%q", got)
	}
}

func TestResolveBlacksmithLeaseID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got, err := resolveBlacksmithLeaseID("tbx_raw123", "/repo", false); err != nil || got != "tbx_raw123" {
		t.Fatalf("raw id got=%q err=%v", got, err)
	}
	if err := claimLeaseForRepoProvider("tbx_abc123", "Blue Lobster", blacksmithTestboxProvider, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	got, err := resolveBlacksmithLeaseID("blue-lobster", "/repo", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "tbx_abc123" {
		t.Fatalf("id=%q", got)
	}
	if _, err := resolveBlacksmithLeaseID("blue-lobster", "/other", false); err == nil || !strings.Contains(err.Error(), "use --reclaim") {
		t.Fatalf("expected repo claim error, got %v", err)
	}
	if got, err := resolveBlacksmithLeaseID("blue-lobster", "/other", true); err != nil || got != "tbx_abc123" {
		t.Fatalf("reclaim got=%q err=%v", got, err)
	}
}

func TestBlacksmithClaimSlugPreservesExistingSlug(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := claimLeaseForRepoProvider("tbx_abc123", "Blue Lobster", blacksmithTestboxProvider, "/repo", time.Minute, false); err != nil {
		t.Fatal(err)
	}
	got, err := blacksmithClaimSlug("tbx_abc123", "tbx_abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Blue Lobster" {
		t.Fatalf("slug=%q", got)
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
