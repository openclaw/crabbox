package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

type jobNoConfigureProvider struct {
	testHetznerProvider
	t *testing.T
}

func (p jobNoConfigureProvider) Configure(Config, Runtime) (Backend, error) {
	p.t.Fatal("job admission configured a backend")
	return nil, nil
}

type jobOptionsTestProvider struct {
	jobNoConfigureProvider
	requests *[]RunRequest
}

func (p jobOptionsTestProvider) ValidateRunOptions(req RunRequest) error {
	*p.requests = append(*p.requests, req)
	return nil
}

func TestJobNoSyncDryRunProviderAdmission(t *testing.T) {
	for _, mode := range []string{"optional", "validator", "unselected"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "config.yaml"))
			var requests []RunRequest
			original := providerRegistry["hetzner"]
			base := jobNoConfigureProvider{t: t}
			var provider Provider = base
			if mode != "optional" {
				provider = jobOptionsTestProvider{jobNoConfigureProvider: base, requests: &requests}
			}
			providerRegistry["hetzner"] = provider
			t.Cleanup(func() { providerRegistry["hetzner"] = original })
			selection := "provider: hetzner\n"
			if mode == "unselected" {
				selection = ""
			}
			config := selection + "jobs:\n  check:\n    noSync: true\n    shell: true\n    command: echo ready\n"
			if err := os.WriteFile(os.Getenv("CRABBOX_CONFIG"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			app := App{Stdout: &stdout, Stderr: &stderr}
			if err := app.Run(context.Background(), []string{"job", "run", "--dry-run", "check"}); err != nil {
				t.Fatalf("dry run: %v\nstderr=%s", err, &stderr)
			}
			var want []RunRequest
			if mode == "validator" {
				want = []RunRequest{{NoSync: true, ShellMode: true, Command: []string{"echo ready"}}}
			}
			if !reflect.DeepEqual(requests, want) {
				t.Fatalf("validation requests=%+v, want %+v", requests, want)
			}
			if !strings.Contains(stdout.String(), "--no-sync --shell -- 'echo ready'") {
				t.Fatalf("supported no-sync job missing from plan: %s", &stdout)
			}
			if mode == "unselected" {
				if err := app.Run(context.Background(), []string{"job", "run", "check"}); err == nil || !strings.Contains(err.Error(), "no provider selected") {
					t.Fatalf("job silently selected a default provider: %v", err)
				}
			}
		})
	}
}

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
      githubRunner: true
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
	if !job.Hydrate.Actions || !job.Hydrate.GitHubRunner || job.Hydrate.WaitTimeout.String() != "45m0s" || job.Hydrate.KeepAliveMinutes != 240 {
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

func TestJobRunDryRunPropagatesArchitecture(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`jobs:
  linux-arm:
    provider: azure
    target: linux
    architecture: arm64
    class: fast
    command: go test ./...
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"job", "run", "--dry-run", "linux-arm"}); err != nil {
		t.Fatalf("job dry-run failed: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"crabbox warmup --provider azure --target linux --class fast --arch arm64 --keep=true",
		"crabbox run --provider azure --target linux --class fast --arch arm64 --id '<lease>' --no-hydrate -- go test ./...",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, got)
		}
	}
}

func TestJobRunInjectsReservedExecutionMetadata(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	logPath := installRecordingSSH(t, dir)
	t.Setenv("CRABBOX_FAKE_SSH_PORT", "22")
	t.Setenv("CRABBOX_FAKE_SSH_PROXY", "1")
	configPath := filepath.Join(dir, ".crabbox.yaml")
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte(`jobs:
  metadata:
    provider: run-env-profile-test
    noSync: true
    command: env
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"job", "run", "--id", "cbx_env_profile_test", "metadata"}); err != nil {
		t.Fatalf("job run failed: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(data)
	for _, want := range []string{"CRABBOX_LEASE_ID=", "cbx_env_profile_test", "CRABBOX_SLUG="} {
		if !strings.Contains(logText, want) {
			t.Fatalf("job command missing %q:\n%s", want, logText)
		}
	}
	if !regexp.MustCompile(`CRABBOX_RUN_ID=.*run_[a-f0-9]{12}`).MatchString(logText) {
		t.Fatalf("job command missing run metadata:\n%s", logText)
	}
	invalidate := strings.Index(logText, `/bin/rm -f -- "$meta_dir/sync-fingerprint"`)
	command := strings.Index(logText, "CRABBOX_LEASE_ID=")
	if invalidate < 0 || command <= invalidate {
		t.Fatalf("job command ran before fingerprint invalidation:\n%s", logText)
	}
}

func TestJobRunDryRunPropagatesEvidenceOptions(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`jobs:
  smoke:
    command: pnpm test
    label: nightly smoke
    artifactGlobs:
      - reports/**
    requiredArtifacts:
      - reports/summary.json
    downloads:
      - reports/summary.json=artifacts/summary.json
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"job", "run", "--dry-run", "--id", "blue-lobster", "smoke"}); err != nil {
		t.Fatalf("job dry-run failed: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{
		"--label 'nightly smoke'",
		"--artifact-glob 'reports/**'",
		"--require-artifact reports/summary.json",
		"--download reports/summary.json=artifacts/summary.json",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, got)
		}
	}
}

func TestJobRunDryRunNoHydratePropagatesToRun(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`actions:
  workflow: .github/workflows/hydrate.yml
jobs:
  test:
    hydrate:
      actions: true
    command: pnpm test
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"job", "run", "--dry-run", "--no-hydrate", "--id", "blue-lobster", "test"}); err != nil {
		t.Fatalf("job dry-run failed: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	if strings.Contains(got, "crabbox actions hydrate") {
		t.Fatalf("--no-hydrate should skip explicit hydrate:\n%s", got)
	}
	if !strings.Contains(got, "crabbox run --id blue-lobster --no-hydrate -- pnpm test") {
		t.Fatalf("--no-hydrate should be passed to nested run:\n%s", got)
	}
}

func TestJobRunDryRunHydrateOptOutDisablesRunAutoHydrate(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`actions:
  workflow: .github/workflows/hydrate.yml
jobs:
  test:
    command: pnpm test
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"job", "run", "--dry-run", "--id", "blue-lobster", "test"}); err != nil {
		t.Fatalf("job dry-run failed: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	if strings.Contains(got, "crabbox actions hydrate") {
		t.Fatalf("hydrate opt-out should skip explicit hydrate:\n%s", got)
	}
	if !strings.Contains(got, "crabbox run --id blue-lobster --no-hydrate -- pnpm test") {
		t.Fatalf("hydrate opt-out should disable nested run auto-hydrate:\n%s", got)
	}
}

func TestJobRunDryRunGitHubRunnerPropagatesToHydrate(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, ".crabbox.yaml"))
	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte(`jobs:
  test:
    hydrate:
      actions: true
    command: pnpm test
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"job", "run", "--dry-run", "--github-runner", "--id", "blue-lobster", "test"}); err != nil {
		t.Fatalf("job dry-run failed: %v\nstderr=%s", err, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "crabbox actions hydrate --id blue-lobster --github-runner") {
		t.Fatalf("--github-runner should be passed to actions hydrate:\n%s", got)
	}
}

func TestParseWarmupLeaseID(t *testing.T) {
	got := parseWarmupLeaseID("leased cbx_123 slug=blue-lobster provider=aws\nwarmup complete total=1s\n")
	if got != "cbx_123" {
		t.Fatalf("parseWarmupLeaseID=%q", got)
	}
}
