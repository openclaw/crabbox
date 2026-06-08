package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitProjectWritesExpectedFiles(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldwd)
	})

	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.Run(context.Background(), []string{"init"}); err != nil {
		t.Fatalf("init error: %v", err)
	}
	for _, path := range []string{
		".crabbox.yaml",
		".github/workflows/crabbox.yml",
		".agents/skills/crabbox/SKILL.md",
	} {
		if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
			t.Fatalf("expected %s: %v", path, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, ".agents/skills/crabbox/SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "crabbox warmup") {
		t.Fatalf("skill missing warmup instructions: %s", data)
	}
	workflow, err := os.ReadFile(filepath.Join(dir, ".github/workflows/crabbox.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"crabbox_job:",
		"ENV_FILE=${env_file}",
		"SERVICES_FILE=${services_file}",
		"GITHUB_JOB",
		"RUNNER_TOOL_CACHE",
	} {
		if !strings.Contains(string(workflow), want) {
			t.Fatalf("workflow missing %q:\n%s", want, workflow)
		}
	}
	config, err := os.ReadFile(filepath.Join(dir, ".crabbox.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "job: hydrate") {
		t.Fatalf("config missing actions job:\n%s", config)
	}
	if err := app.Run(context.Background(), []string{"init"}); err == nil {
		t.Fatal("second init without --force succeeded")
	}
}

func TestInitProjectDetectsRepoCommands(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldwd)
	})

	mustWrite := func(path, content string) {
		t.Helper()
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("go.mod", "module example.com/my-app\n\ngo 1.24\n")
	mustWrite("package.json", `{"scripts":{"check":"node --test ./test/*.js"}}`)
	mustWrite("worker/package.json", `{"scripts":{"test":"vitest run"}}`)
	mustWrite("worker/package-lock.json", `{"lockfileVersion": 3}`)

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run(context.Background(), []string{"init", "--detect"}); err != nil {
		t.Fatalf("init --detect error: %v", err)
	}
	if !strings.Contains(stdout.String(), "detected job: crabbox job run detected") {
		t.Fatalf("stdout missing detected job hint:\n%s", stdout.String())
	}
	config, err := os.ReadFile(filepath.Join(dir, ".crabbox.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	configText := string(config)
	for _, want := range []string{
		"run:\n  preflightTools:",
		"- go",
		"- node",
		"- npm",
		"jobs:\n  detected:",
		"shell: true",
		"go test ./...",
		"npm install && npm run 'check' &&",
		"(cd 'worker' && npm ci && npm test)",
	} {
		if !strings.Contains(configText, want) {
			t.Fatalf("detected config missing %q:\n%s", want, configText)
		}
	}
	skill, err := os.ReadFile(filepath.Join(dir, ".agents/skills/crabbox/SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(skill), "crabbox job run detected") {
		t.Fatalf("skill missing detected job hint:\n%s", skill)
	}

	fileCfg, err := readFileConfig(filepath.Join(dir, ".crabbox.yaml"))
	if err != nil {
		t.Fatalf("generated config should parse: %v", err)
	}
	loaded := baseConfig()
	applyFileConfig(&loaded, fileCfg)
	if _, ok := loaded.Jobs["detected"]; !ok {
		t.Fatalf("generated config missing detected job: %#v", loaded.Jobs)
	}
	if err := validatePreflightTools(loaded.Run.PreflightTools); err != nil {
		t.Fatalf("generated preflight tools should validate: %v", err)
	}
}

func TestWriteInitFileBranches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "file.txt")
	if err := writeInitFile(path, "first", false); err != nil {
		t.Fatal(err)
	}
	if err := writeInitFile(path, "second", false); err == nil {
		t.Fatal("expected existing file error")
	}
	if err := writeInitFile(path, "second", true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "second" {
		t.Fatalf("content=%q", data)
	}

	parent := filepath.Join(t.TempDir(), "not-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeInitFile(filepath.Join(parent, "file.txt"), "x", false); err == nil {
		t.Fatal("expected create directory error")
	}

	dirPath := t.TempDir()
	if err := writeInitFile(dirPath, "x", true); err == nil {
		t.Fatal("expected write directory error")
	}
}

func TestSubcommandHelpExitsZero(t *testing.T) {
	var stderr bytes.Buffer
	app := App{Stdout: &bytes.Buffer{}, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"init", "--help"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
		t.Fatalf("init --help error=%v, want exit 0", err)
	}
	if !strings.Contains(stderr.String(), "Usage of init") {
		t.Fatalf("init --help output missing usage: %s", stderr.String())
	}
}

func TestPassthroughCommandHelpExitsBeforeExecution(t *testing.T) {
	for _, command := range []string{"warmup", "run", "status", "ssh", "ports", "cp", "vnc", "webvnc", "screenshot", "inspect", "stop"} {
		t.Run(command, func(t *testing.T) {
			var stderr bytes.Buffer
			app := App{Stdout: &bytes.Buffer{}, Stderr: &stderr}
			err := app.Run(context.Background(), []string{command, "--help"})
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
				t.Fatalf("%s --help error=%v, want exit 0", command, err)
			}
			if !strings.Contains(stderr.String(), "Usage") {
				t.Fatalf("%s --help output missing usage: %s", command, stderr.String())
			}
		})
	}
}

func TestGroupedCommandHelpExitsZero(t *testing.T) {
	for _, command := range []string{"actions", "admin", "cache", "config", "desktop", "pool", "machine"} {
		t.Run(command, func(t *testing.T) {
			for _, args := range [][]string{
				{command, "--help"},
				{command, "help"},
				{command},
			} {
				var stdout bytes.Buffer
				app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
				err := app.Run(context.Background(), args)
				if err != nil {
					t.Fatalf("%v error=%v, want nil", args, err)
				}
				if !strings.Contains(stdout.String(), "Usage:") {
					t.Fatalf("%v output missing usage: %s", args, stdout.String())
				}
			}
		})
	}
}

func TestHelpSubcommandRoutesToCommandHelp(t *testing.T) {
	var stderr bytes.Buffer
	app := App{Stdout: &bytes.Buffer{}, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"help", "run"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
		t.Fatalf("help run error=%v, want exit 0", err)
	}
	if !strings.Contains(stderr.String(), "Usage of run") {
		t.Fatalf("help run output missing usage: %s", stderr.String())
	}
}

func TestTopLevelHelpIsWorkflowFirst(t *testing.T) {
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run(context.Background(), []string{"help"}); err != nil {
		t.Fatalf("help error: %v", err)
	}
	for _, want := range []string{
		"Start Here:",
		"Commands:",
		"Common Flows:",
		"crabbox run --id blue-lobster -- pnpm test:changed",
		"Aliases:",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("help output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestKongRouterPreservesVersionAndUsageExitCodes(t *testing.T) {
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.Run(context.Background(), []string{"--version"}); err != nil {
		t.Fatalf("--version error: %v", err)
	}
	if strings.TrimSpace(stdout.String()) != currentVersion() {
		t.Fatalf("--version output=%q, want %q", stdout.String(), currentVersion())
	}

	err := app.Run(context.Background(), []string{"nope"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("unknown command error=%v, want exit 2", err)
	}
}
