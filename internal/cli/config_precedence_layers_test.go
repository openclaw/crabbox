package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigSelectorLayers(t *testing.T) {
	for _, tc := range []struct {
		name, repo, envTarget, envMode, envClass string
		flags                                    []string
		wantTarget, wantMode, wantClass          string
	}{
		{"user", "", "", "", "", nil, targetMacOS, windowsModeNormal, "tiny"},
		{"repo", "target: windows\nwindows:\n  mode: wsl2\nclass: small\n", "", "", "", nil, targetWindows, windowsModeWSL2, "small"},
		{"env", "target: macos\nclass: small\n", "windows", "normal", "standard", nil, targetWindows, windowsModeNormal, "standard"},
		{"flags", "target: macos\nclass: small\n", "windows", "wsl2", "standard", []string{"--target", "linux", "--class", "large"}, targetLinux, windowsModeNormal, "large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Chdir(t.TempDir())
			t.Setenv("CRABBOX_CONFIG", "")
			var observed Config
			provider := precedenceAuditProvider{observed: &observed}
			providerRegistry[provider.Spec().Name] = provider
			t.Cleanup(func() { delete(providerRegistry, provider.Spec().Name) })
			userPath := userConfigPath()
			if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(userPath, []byte("provider: precedence-audit\ntarget: macos\nclass: tiny\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.repo != "" {
				if err := os.WriteFile("crabbox.yaml", []byte(tc.repo), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CRABBOX_TARGET", tc.envTarget)
			t.Setenv("CRABBOX_WINDOWS_MODE", tc.envMode)
			t.Setenv("CRABBOX_DEFAULT_CLASS", tc.envClass)
			fs := newFlagSet("layer audit", io.Discard)
			values := registerLeaseCreateFlags(fs, defaultConfig())
			if err := fs.Parse(tc.flags); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfigForFlags(fs)
			if err != nil {
				t.Fatal(err)
			}
			if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if cfg.TargetOS != tc.wantTarget || cfg.WindowsMode != tc.wantMode || cfg.Class != tc.wantClass {
				t.Fatalf("target=%s mode=%s class=%s", cfg.TargetOS, cfg.WindowsMode, cfg.Class)
			}
		})
	}
}
