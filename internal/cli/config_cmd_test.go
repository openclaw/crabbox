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
