package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigShowIncludesRunPreflightTools(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("run:\n  preflightTools: [node, bun]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "run preflight_tools=node,bun") {
		t.Fatalf("config show missing run preflight tools: %q", stdout.String())
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Run struct {
			PreflightTools []string `json:"preflightTools"`
		} `json:"run"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Run.PreflightTools, ",") != "node,bun" {
		t.Fatalf("json run.preflightTools=%v", got.Run.PreflightTools)
	}
}

func TestConfigShowIncludesJobHydrateGitHubRunner(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("jobs:\n  smoke:\n    architecture: arm64\n    hydrate:\n      actions: true\n      githubRunner: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Jobs map[string]struct {
			Architecture string `json:"architecture"`
			Hydrate      struct {
				GitHubRunner bool `json:"githubRunner"`
			} `json:"hydrate"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Jobs["smoke"].Hydrate.GitHubRunner {
		t.Fatalf("json jobs.smoke.hydrate.githubRunner=false in %s", stdout.String())
	}
	if got.Jobs["smoke"].Architecture != "arm64" {
		t.Fatalf("json jobs.smoke.architecture=%q in %s", got.Jobs["smoke"].Architecture, stdout.String())
	}
}

func TestConfigShowIncludesCloudflareWithoutSecret(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_CLOUDFLARE_RUNNER_TOKEN", "cloudflare-secret-token")
	if err := os.WriteFile(configPath, []byte("cloudflare:\n  apiUrl: https://cloudflare.example.test\n  workdir: /workspace/test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	if !strings.Contains(text, "cloudflare api_url=https://cloudflare.example.test workdir=/workspace/test auth=configured") {
		t.Fatalf("config show missing cloudflare summary: %q", text)
	}
	if strings.Contains(text, "cloudflare-secret-token") {
		t.Fatalf("config show leaked Cloudflare token: %q", text)
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Cloudflare struct {
			APIURL  string `json:"apiUrl"`
			Auth    string `json:"auth"`
			Workdir string `json:"workdir"`
		} `json:"cloudflare"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Cloudflare.APIURL != "https://cloudflare.example.test" || got.Cloudflare.Workdir != "/workspace/test" || got.Cloudflare.Auth != "configured" {
		t.Fatalf("unexpected cloudflare json: %#v", got.Cloudflare)
	}
	if strings.Contains(stdout.String(), "cloudflare-secret-token") {
		t.Fatalf("config show json leaked Cloudflare token: %q", stdout.String())
	}
}

func TestConfigShowIncludesSyncInclude(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("sync:\n  include:\n    - src\n    - scripts\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "includes=2") {
		t.Fatalf("config show text missing includes count: %q", stdout.String())
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Sync struct {
			Include []string `json:"include"`
		} `json:"sync"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sync.Include) != 2 || got.Sync.Include[0] != "src" || got.Sync.Include[1] != "scripts" {
		t.Fatalf("config show json sync.include = %#v, want [src scripts]", got.Sync.Include)
	}
}

func TestConfigShowIncludesDockerSandboxConfig(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", "")
	if err := os.WriteFile(configPath, []byte(`provider: docker-sandbox
dockerSandbox:
  cliPath: /opt/sbx
  agent: shell
  template: ubuntu
  cpus: 2
  memory: 4g
  clone: true
  workdir: /workspace/my-app
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES", "/tmp/extra")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_MCP", "context7,all")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_KIT", "example-org/base")

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err != nil {
		t.Fatal(err)
	}
	text := stdout.String()
	for _, want := range []string{
		"provider=docker-sandbox",
		"docker_sandbox cli=/opt/sbx agent=shell template=ubuntu cpus=2 memory=4g clone=true workdir=/workspace/my-app",
		"extra_workspaces=/tmp/extra",
		"mcp=context7,all",
		"kit=example-org/base",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config show text missing %q: %q", want, text)
		}
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Provider      string `json:"provider"`
		DockerSandbox struct {
			CLIPath         string   `json:"cliPath"`
			Agent           string   `json:"agent"`
			Template        string   `json:"template"`
			CPUs            float64  `json:"cpus"`
			Memory          string   `json:"memory"`
			Clone           bool     `json:"clone"`
			Workdir         string   `json:"workdir"`
			ExtraWorkspaces []string `json:"extraWorkspaces"`
			MCP             []string `json:"mcp"`
			Kit             []string `json:"kit"`
		} `json:"dockerSandbox"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Provider != "docker-sandbox" || got.DockerSandbox.CLIPath != "/opt/sbx" || got.DockerSandbox.Agent != "shell" || got.DockerSandbox.Template != "ubuntu" || got.DockerSandbox.CPUs != 2 || got.DockerSandbox.Memory != "4g" || !got.DockerSandbox.Clone || got.DockerSandbox.Workdir != "/workspace/my-app" {
		t.Fatalf("unexpected dockerSandbox json: %#v", got)
	}
	if strings.Join(got.DockerSandbox.ExtraWorkspaces, ",") != "/tmp/extra" || strings.Join(got.DockerSandbox.MCP, ",") != "context7,all" || strings.Join(got.DockerSandbox.Kit, ",") != "example-org/base" {
		t.Fatalf("unexpected dockerSandbox lists: %#v", got.DockerSandbox)
	}
}

func TestConfigShowRejectsInvalidDockerSandboxCPUConfig(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", "docker-sandbox")
	if err := os.WriteFile(configPath, []byte("profile: default\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CLI", "/opt/docker-sbx")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_AGENT", "shell")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_TEMPLATE", "ubuntu")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CPUS", "2.5")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_MEMORY", "6g")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_CLONE", "true")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_WORKDIR", "/workspace/my-app")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_EXTRA_WORKSPACES", "/tmp/extra")
	t.Setenv("CRABBOX_DOCKER_SANDBOX_KIT", "example-org/base")

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if err := app.configShow(nil); err == nil || !strings.Contains(err.Error(), "docker-sandbox cpus must be a whole number") {
		t.Fatalf("configShow err=%v, want docker-sandbox whole-number validation", err)
	}

	stdout.Reset()
	if err := app.configShow([]string{"--json"}); err == nil || !strings.Contains(err.Error(), "docker-sandbox cpus must be a whole number") {
		t.Fatalf("configShow --json err=%v, want docker-sandbox whole-number validation", err)
	}
}
