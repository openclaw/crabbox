package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobSelectorPrecedence(t *testing.T) {
	for _, tc := range []struct{ name, envName, envValue string }{
		{"target", "CRABBOX_TARGET", "obsolete"},
		{"mode", "CRABBOX_WINDOWS_MODE", "wsl2"},
		{"provider", "CRABBOX_PROVIDER", "missing-provider"},
		{"architecture", "CRABBOX_ARCH", "obsolete"},
	} {
		for _, mode := range []string{"create", "reuse", "dry-run"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				clearConfigEnv(t)
				root := t.TempDir()
				t.Chdir(root)
				config := filepath.Join(root, "config.yaml")
				if err := os.WriteFile(config, []byte("class: tiny\njobs:\n  audit:\n    provider: precedence-audit\n    target: linux\n    architecture: amd64\n    command: 'true'\n    noSync: true\n    stop: never\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CRABBOX_CONFIG", config)
				t.Setenv(tc.envName, tc.envValue)
				var observed Config
				provider := precedenceAuditProvider{observed: &observed}
				providerRegistry[provider.Spec().Name] = provider
				t.Cleanup(func() { delete(providerRegistry, provider.Spec().Name) })
				args := []string{"audit"}
				if mode == "reuse" {
					args = []string{"--id", "cbx_0123456789ab", "audit"}
				}
				if mode == "dry-run" {
					args = []string{"--dry-run", "audit"}
				}
				var output bytes.Buffer
				err := (App{Stdout: &output, Stderr: io.Discard}).jobRun(t.Context(), args)
				if mode == "dry-run" {
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(output.String(), "--target linux") || !strings.Contains(output.String(), "--arch amd64") {
						t.Fatalf("unexpected plan: %s", &output)
					}
					if observed.Provider != "" {
						t.Fatal("dry run configured a provider")
					}
					return
				}
				if !errors.Is(err, errPrecedenceBoundary) {
					t.Fatalf("did not reach provider boundary: %v", err)
				}
				if observed.Provider != provider.Spec().Name || observed.TargetOS != targetLinux || observed.Architecture != ArchitectureAMD64 || observed.WindowsMode != windowsModeNormal || observed.Class != "tiny" {
					t.Fatalf("resolved provider=%s target=%s architecture=%s mode=%s class=%s", observed.Provider, observed.TargetOS, observed.Architecture, observed.WindowsMode, observed.Class)
				}
			})
		}
	}
}
