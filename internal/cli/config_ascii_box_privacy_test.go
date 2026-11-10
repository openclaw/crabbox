package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestAsciiBoxPrivacyConfig(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.AsciiBox.NoSnapshots || cfg.AsciiBox.Visibility != "" {
		t.Fatal("privacy overrides must default to unset")
	}
	noSnapshots, visibility := true, "private"
	if err := applyFileConfig(&cfg, fileConfig{AsciiBox: &fileAsciiBoxConfig{NoSnapshots: &noSnapshots, Visibility: &visibility}}); err != nil {
		t.Fatal(err)
	}
	view := configShowView(cfg)["asciiBox"].(map[string]any)
	if view["noSnapshots"] != true || view["visibility"] != "private" || cfg.AsciiBox.NoEnv {
		t.Fatalf("privacy config: %v", view)
	}
	var shown bytes.Buffer
	if err := writeConfigShowText(&shown, cfg); err != nil || !strings.Contains(shown.String(), "no_snapshots=true visibility=private") {
		t.Fatalf("text config: %v", err)
	}
	noSnapshots, visibility = false, ""
	if err := applyFileConfig(&cfg, fileConfig{AsciiBox: &fileAsciiBoxConfig{NoSnapshots: &noSnapshots, Visibility: &visibility}}); err != nil || cfg.AsciiBox.NoSnapshots || cfg.AsciiBox.Visibility != "" {
		t.Fatalf("explicit defaults: %+v %v", cfg.AsciiBox, err)
	}
}
