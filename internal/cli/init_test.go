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
	for _, command := range []string{"warmup", "run", "status", "ssh", "vnc", "webvnc", "screenshot", "inspect", "stop"} {
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
