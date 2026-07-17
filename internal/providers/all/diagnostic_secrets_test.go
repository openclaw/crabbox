package all

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestRuntimeOnlyProviderDiagnosticSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, ".oc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".oc", "config.json"), []byte(`{"api_key":"opencomputer-file-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte("machine api.wandb.ai login test password wandb-netrc-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	environment := map[string]string{
		"CRABBOX_OPENCOMPUTER_API_KEY": "opencomputer-primary-secret",
		"OPENCOMPUTER_API_KEY":         "opencomputer-fallback-secret",
		"CRABBOX_CODESANDBOX_API_KEY":  "codesandbox-primary-secret",
		"CSB_API_KEY":                  "codesandbox-fallback-secret",
		"CRABBOX_OPENSANDBOX_API_KEY":  "opensandbox-primary-secret",
		"OPEN_SANDBOX_API_KEY":         "opensandbox-fallback-secret",
		"CRABBOX_SUPERSERVE_API_KEY":   "superserve-primary-secret",
		"SUPERSERVE_API_KEY":           "superserve-fallback-secret",
		"CRABBOX_CROWNEST_API_KEY":     "crownest-primary-secret",
		"CROWNEST_API_KEY":             "crownest-fallback-secret",
		"CRABBOX_WANDB_API_KEY":        "wandb-primary-secret",
		"WANDB_API_KEY":                "wandb-fallback-secret",
		"EXTERNAL_DESKTOP_PASSWORD":    "external-desktop-secret",
	}
	for name, value := range environment {
		t.Setenv(name, value)
	}

	tests := []struct {
		provider string
		want     []string
	}{
		{"opencomputer", []string{environment["CRABBOX_OPENCOMPUTER_API_KEY"], environment["OPENCOMPUTER_API_KEY"], "opencomputer-file-secret"}},
		{"codesandbox", []string{environment["CRABBOX_CODESANDBOX_API_KEY"], environment["CSB_API_KEY"]}},
		{"opensandbox", []string{environment["CRABBOX_OPENSANDBOX_API_KEY"], environment["OPEN_SANDBOX_API_KEY"]}},
		{"superserve", []string{environment["CRABBOX_SUPERSERVE_API_KEY"], environment["SUPERSERVE_API_KEY"]}},
		{"crownest", []string{environment["CRABBOX_CROWNEST_API_KEY"], environment["CROWNEST_API_KEY"]}},
		{"wandb", []string{environment["CRABBOX_WANDB_API_KEY"], "wandb-config-secret", environment["WANDB_API_KEY"], "wandb-netrc-secret"}},
		{"external", []string{environment["EXTERNAL_DESKTOP_PASSWORD"]}},
	}
	cfg := core.Config{
		Wandb: core.WandbConfig{APIKey: "wandb-config-secret"},
		External: core.ExternalConfig{
			Connection: core.ExternalConnectionConfig{
				Desktop: core.ExternalDesktopConfig{PasswordEnv: "EXTERNAL_DESKTOP_PASSWORD"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			provider, err := core.ProviderFor(test.provider)
			if err != nil {
				t.Fatal(err)
			}
			source, ok := provider.(core.ProviderDiagnosticSecretSource)
			if !ok {
				t.Fatalf("provider %s does not contribute runtime diagnostic secrets", test.provider)
			}
			got := source.DiagnosticSecrets(cfg)
			for _, want := range test.want {
				if !slices.Contains(got, want) {
					t.Fatalf("DiagnosticSecrets()=%q missing expected value", got)
				}
			}
		})
	}
}
