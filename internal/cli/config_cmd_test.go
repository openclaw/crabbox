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
