package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestConfigShowIncludesLume(t *testing.T) {
	cfg := baseConfig()
	cfg.Lume.CLIPath = "/opt/homebrew/bin/lume"
	cfg.Lume.Base = "macos-golden"
	cfg.Lume.Storage = "fast"
	cfg.Lume.User = "builder"
	cfg.Lume.WorkRoot = "/Users/builder/work"
	view, ok := configShowView(cfg)["lume"].(map[string]any)
	if !ok || view["cliPath"] != "/opt/homebrew/bin/lume" || view["base"] != "macos-golden" || view["storage"] != "fast" || view["workRoot"] != "/Users/builder/work" {
		t.Fatalf("lume view=%#v", view)
	}
	var text bytes.Buffer
	writeConfigShowText(&text, cfg)
	if !strings.Contains(text.String(), "lume cli=/opt/homebrew/bin/lume base=macos-golden storage=fast user=builder work_root=/Users/builder/work") {
		t.Fatalf("config show missing Lume settings: %q", text.String())
	}
}
