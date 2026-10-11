package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestTemplateDefaultsPreserveSelectorFlags(t *testing.T) {
	for _, tc := range []struct {
		name, template       string
		flags                []string
		wantTarget, wantMode string
	}{
		{"target clears template mode", "windowsMode: wsl2", []string{"--target", "linux"}, targetLinux, windowsModeNormal},
		{"target beats template target", "target: macos", []string{"--target", "linux"}, targetLinux, windowsModeNormal},
		{"mode beats template mode", "windowsMode: wsl2", []string{"--windows-mode", "normal"}, targetWindows, windowsModeNormal},
		{"unasserted mode uses template", "windowsMode: wsl2", []string{"--target", "windows"}, targetWindows, windowsModeWSL2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			config := filepath.Join(t.TempDir(), "config.yaml")
			data := "provider: parallels\ntarget: windows\nparallels:\n  template: audit\n  templates:\n    audit:\n      " + tc.template + "\n"
			if err := os.WriteFile(config, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", config)
			fs := newFlagSet("status", io.Discard)
			flags := registerTargetFlags(fs, defaultConfig())
			if err := fs.Parse(tc.flags); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadLeaseTargetConfig(fs, "parallels", flags, networkModeFlagValues{}, leaseTargetConfigOptions{LeaseID: "cbx_0123456789ab"})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.TargetOS != tc.wantTarget || cfg.WindowsMode != tc.wantMode {
				t.Fatalf("target=%s mode=%s, want %s/%s", cfg.TargetOS, cfg.WindowsMode, tc.wantTarget, tc.wantMode)
			}
		})
	}
}
