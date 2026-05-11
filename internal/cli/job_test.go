package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigJobs(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`jobs:
  openclaw-wsl2:
    provider: aws
    target: windows
    windows:
      mode: wsl2
    class: beast
    market: on-demand
    idleTimeout: 240m
    hydrate:
      actions: true
      waitTimeout: 45m
      keepAliveMinutes: 240
    actions:
      workflow: hydrate.yml
      job: hydrate
      fields:
        - suite=full
    shell: true
    command: pnpm test
    stop: always
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	job := cfg.Jobs["openclaw-wsl2"]
	if job.Provider != "aws" || job.Target != "windows" || job.WindowsMode != "wsl2" || job.Class != "beast" || job.Market != "on-demand" {
		t.Fatalf("job target/capacity not loaded: %#v", job)
	}
	if !job.Hydrate.Actions || job.Hydrate.WaitTimeout.String() != "45m0s" || job.Hydrate.KeepAliveMinutes != 240 {
		t.Fatalf("job hydrate not loaded: %#v", job.Hydrate)
	}
	if job.Actions.Workflow != "hydrate.yml" || job.Actions.Job != "hydrate" || len(job.Actions.Fields) != 1 {
		t.Fatalf("job actions not loaded: %#v", job.Actions)
	}
	if !job.Shell || job.Command != "pnpm test" || job.Stop != "always" {
		t.Fatalf("job command not loaded: %#v", job)
	}
}

func TestJobRunDryRunBuildsOrchestrationCommands(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`jobs:
  openclaw-wsl2:
    provider: aws
    target: windows
    windows:
      mode: wsl2
    class: beast
    market: on-demand
    idleTimeout: 240m
    hydrate:
      actions: true
      waitTimeout: 45m
      keepAliveMinutes: 240
    actions:
      workflow: hydrate.yml
      job: hydrate
    shell: true
    command: pnpm install --frozen-lockfile && pnpm test
    stop: always
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"job", "run", "--dry-run", "openclaw-wsl2"}); err != nil {
		t.Fatalf("job dry-run failed: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"crabbox warmup --provider aws --target windows --windows-mode wsl2 --class beast --market on-demand --idle-timeout 4h0m0s --keep=true",
		"crabbox actions hydrate --provider aws --target windows --windows-mode wsl2 --id '<lease>' --workflow hydrate.yml --job hydrate --wait-timeout 45m0s --keep-alive-minutes 240",
		"crabbox run --provider aws --target windows --windows-mode wsl2 --class beast --market on-demand --idle-timeout 4h0m0s --id '<lease>' --shell -- 'pnpm install --frozen-lockfile && pnpm test'",
		"crabbox stop --provider aws --target windows --windows-mode wsl2 <lease>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, got)
		}
	}
}

func TestParseWarmupLeaseID(t *testing.T) {
	got := parseWarmupLeaseID("leased cbx_123 slug=blue-lobster provider=aws\nwarmup complete total=1s\n")
	if got != "cbx_123" {
		t.Fatalf("parseWarmupLeaseID=%q", got)
	}
}
