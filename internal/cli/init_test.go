package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

var agentSkillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

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
	assertValidAgentSkill(t, filepath.Join(dir, ".agents/skills/crabbox/SKILL.md"), data)
	skillText := string(data)
	for _, want := range []string{
		"# Crabbox",
		"target-platform coverage",
		"crabbox warmup",
		"crabbox stop <slug>",
	} {
		if !strings.Contains(skillText, want) {
			t.Fatalf("skill missing %q: %s", want, data)
		}
	}
	workflow, err := os.ReadFile(filepath.Join(dir, ".github/workflows/crabbox.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"crabbox_job:",
		"CRABBOX_ID: ${{ inputs.crabbox_id }}",
		"CRABBOX_JOB: ${{ inputs.crabbox_job }}",
		"CRABBOX_KEEP_ALIVE_MINUTES: ${{ inputs.crabbox_keep_alive_minutes }}",
		"ENV_FILE=${env_file}",
		"SERVICES_FILE=${services_file}",
		"GITHUB_JOB",
		"RUNNER_TOOL_CACHE",
	} {
		if !strings.Contains(string(workflow), want) {
			t.Fatalf("workflow missing %q:\n%s", want, workflow)
		}
	}
	for _, blocked := range []string{
		`job="${{ inputs.crabbox_job }}"`,
		`${{ inputs.crabbox_id }}.env`,
		`minutes="${{ inputs.crabbox_keep_alive_minutes }}"`,
		`${{ inputs.crabbox_id }}.stop`,
	} {
		if strings.Contains(string(workflow), blocked) {
			t.Fatalf("workflow contains unsafe interpolation %q:\n%s", blocked, workflow)
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

func assertValidAgentSkill(t *testing.T, skillPath string, data []byte) {
	t.Helper()

	const opening = "---\n"
	if !bytes.HasPrefix(data, []byte(opening)) {
		t.Fatalf("skill frontmatter must begin at byte zero:\n%s", data)
	}
	remainder := data[len(opening):]
	closing := bytes.Index(remainder, []byte("\n---\n"))
	if closing < 0 {
		t.Fatalf("skill frontmatter is not closed:\n%s", data)
	}

	var metadata struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		License     string `yaml:"license"`
	}
	if err := yaml.Unmarshal(remainder[:closing], &metadata); err != nil {
		t.Fatalf("parse skill frontmatter: %v", err)
	}
	if metadata.Name != "crabbox" {
		t.Fatalf("skill name=%q, want crabbox", metadata.Name)
	}
	nameLength := utf8.RuneCountInString(metadata.Name)
	if nameLength < 1 || nameLength > 64 || !agentSkillNamePattern.MatchString(metadata.Name) {
		t.Fatalf("invalid skill name %q", metadata.Name)
	}
	if metadata.Name != filepath.Base(filepath.Dir(skillPath)) {
		t.Fatalf("skill name %q does not match parent directory %q", metadata.Name, filepath.Base(filepath.Dir(skillPath)))
	}
	if metadata.License != "MIT" {
		t.Fatalf("skill license=%q, want MIT", metadata.License)
	}
	descriptionLength := utf8.RuneCountInString(metadata.Description)
	if metadata.Description != strings.TrimSpace(metadata.Description) || descriptionLength < 1 || descriptionLength > 1024 {
		t.Fatalf("invalid skill description %q", metadata.Description)
	}
	for _, want := range []string{"Detect and use Crabbox", "Use when", "crabbox.yaml", ".crabbox.yaml", "crabbox CLI"} {
		if !strings.Contains(metadata.Description, want) {
			t.Fatalf("skill description missing %q: %q", want, metadata.Description)
		}
	}
	body := bytes.TrimSpace(remainder[closing+len("\n---\n"):])
	if len(body) == 0 {
		t.Fatal("skill Markdown body is empty")
	}
}

func TestNormalizeInitSkillPath(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "default", path: ".agents/skills/crabbox/SKILL.md", want: filepath.FromSlash(".agents/skills/crabbox/SKILL.md")},
		{name: "documented Claude path", path: ".claude/skills/crabbox/SKILL.md", want: filepath.FromSlash(".claude/skills/crabbox/SKILL.md")},
		{name: "leading dot", path: "./.agents/skills/crabbox/SKILL.md", want: filepath.FromSlash(".agents/skills/crabbox/SKILL.md")},
		{name: "wrong parent", path: ".agents/skills/remote/SKILL.md", wantErr: true},
		{name: "wrong filename", path: ".agents/skills/crabbox/crabbox.md", wantErr: true},
		{name: "outside repository", path: "../crabbox/SKILL.md", wantErr: true},
		{name: "absolute", path: filepath.Join(string(filepath.Separator), "tmp", "crabbox", "SKILL.md"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeInitSkillPath(test.path)
			if test.wantErr {
				if err == nil {
					t.Fatalf("normalizeInitSkillPath(%q) succeeded with %q", test.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeInitSkillPath(%q): %v", test.path, err)
			}
			if got != test.want {
				t.Fatalf("normalizeInitSkillPath(%q)=%q, want %q", test.path, got, test.want)
			}
		})
	}
}

func TestInitProjectWritesRepeatedSkillPaths(t *testing.T) {
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

	paths := []string{
		".agents/skills/crabbox/SKILL.md",
		".claude/skills/crabbox/SKILL.md",
	}
	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.Run(context.Background(), []string{
		"init",
		"--skill", paths[0],
		"--skill", paths[1],
	}); err != nil {
		t.Fatalf("init with repeated --skill: %v", err)
	}
	for _, skillPath := range paths {
		data, err := os.ReadFile(filepath.Join(dir, skillPath))
		if err != nil {
			t.Fatalf("read %s: %v", skillPath, err)
		}
		assertValidAgentSkill(t, filepath.Join(dir, skillPath), data)
	}
}

func TestInitProjectPreflightsEveryTargetBeforeWriting(t *testing.T) {
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

	if err := os.WriteFile(filepath.Join(dir, ".crabbox.yaml"), []byte("existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	err = app.Run(context.Background(), []string{
		"init",
		"--skill", ".agents/skills/crabbox/SKILL.md",
		"--skill", ".claude/skills/crabbox/SKILL.md",
	})
	if err == nil {
		t.Fatal("init succeeded with an existing target")
	}
	for _, path := range []string{
		".github/workflows/crabbox.yml",
		".agents/skills/crabbox/SKILL.md",
		".claude/skills/crabbox/SKILL.md",
	} {
		if _, statErr := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(statErr) {
			t.Fatalf("init wrote %s before reporting the existing target: %v", path, statErr)
		}
	}
}

func TestInitProjectRejectsAliasedSkillPathsBeforeWriting(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		paths []string
	}{
		{
			name:  "case-only alias",
			setup: func(_ *testing.T, _ string) {},
			paths: []string{
				".agents/skills/crabbox/SKILL.md",
				".AGENTS/skills/crabbox/SKILL.md",
			},
		},
		{
			name: "symlinked root",
			setup: func(t *testing.T, dir string) {
				agentsSkills := filepath.Join(dir, ".agents", "skills")
				if err := os.MkdirAll(agentsSkills, 0o755); err != nil {
					t.Fatal(err)
				}
				claudeDir := filepath.Join(dir, ".claude")
				if err := os.MkdirAll(claudeDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join("..", ".agents", "skills"), filepath.Join(claudeDir, "skills")); err != nil {
					t.Skipf("create skill-root symlink: %v", err)
				}
			},
			paths: []string{
				".agents/skills/crabbox/SKILL.md",
				".claude/skills/crabbox/SKILL.md",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			oldwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(dir); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(oldwd) })
			test.setup(t, dir)

			app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
			err = app.Run(context.Background(), []string{
				"init",
				"--skill", test.paths[0],
				"--skill", test.paths[1],
			})
			if err == nil || !strings.Contains(err.Error(), "init target path is repeated") {
				t.Fatalf("init error=%v, want repeated target error", err)
			}
			for _, path := range []string{
				".crabbox.yaml",
				".github/workflows/crabbox.yml",
				test.paths[0],
			} {
				if _, statErr := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(statErr) {
					t.Fatalf("init wrote %s before rejecting aliases: %v", path, statErr)
				}
			}
		})
	}
}

func TestInitProjectRejectsHardLinkedTargetsBeforeForceWriting(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	paths := []string{
		".agents/skills/crabbox/SKILL.md",
		".claude/skills/crabbox/SKILL.md",
	}
	first := filepath.Join(dir, paths[0])
	second := filepath.Join(dir, paths[1])
	if err := os.MkdirAll(filepath.Dir(first), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(second), 0o755); err != nil {
		t.Fatal(err)
	}
	const original = "existing skill\n"
	if err := os.WriteFile(first, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Skipf("create hard-linked skill target: %v", err)
	}

	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	err = app.Run(context.Background(), []string{
		"init", "--force",
		"--skill", paths[0],
		"--skill", paths[1],
	})
	if err == nil || !strings.Contains(err.Error(), "init target path is repeated") {
		t.Fatalf("init error=%v, want repeated target error", err)
	}
	for _, path := range []string{".crabbox.yaml", ".github/workflows/crabbox.yml"} {
		if _, statErr := os.Stat(filepath.Join(dir, path)); !os.IsNotExist(statErr) {
			t.Fatalf("init wrote %s before rejecting hard links: %v", path, statErr)
		}
	}
	for _, path := range paths {
		data, readErr := os.ReadFile(filepath.Join(dir, path))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(data) != original {
			t.Fatalf("init changed %s before rejecting hard links: %q", path, data)
		}
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
