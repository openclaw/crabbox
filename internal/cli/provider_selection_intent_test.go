package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderSelectionIsActionable(t *testing.T) {
	for _, tt := range []struct {
		name   string
		source providerSelectionSource
		want   bool
	}{
		{name: "empty source", source: providerSelectionSource("")},
		{name: "unknown source", source: providerSelectionSource("future_source")},
		{name: "compiled default", source: providerSelectionCompiledDefault},
		{name: "user config", source: providerSelectionUserConfig, want: true},
		{name: "repo config", source: providerSelectionRepoConfig, want: true},
		{name: "environment", source: providerSelectionEnvironment, want: true},
		{name: "flag", source: providerSelectionFlag, want: true},
		{name: "recorded run", source: providerSelectionRecordedRun, want: true},
		{name: "lease context", source: providerSelectionLeaseContext, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Provider: "ssh", providerSelectionSource: tt.source}
			if got := providerSelectionIsActionable(cfg); got != tt.want {
				t.Fatalf("providerSelectionIsActionable()=%t want %t", got, tt.want)
			}
		})
	}
	if providerSelectionIsActionable(Config{providerSelectionSource: providerSelectionFlag}) {
		t.Fatal("empty provider must not be actionable")
	}
}

func TestExplicitHetznerWarmupReachesCredentialValidation(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("HCLOUD_TOKEN", "")
	t.Setenv("HETZNER_TOKEN", "")
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).warmup(context.Background(), []string{"--provider", "hetzner"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 3 || !strings.Contains(exitErr.Message, "HCLOUD_TOKEN or HETZNER_TOKEN is required") {
		t.Fatalf("explicit Hetzner error=%v, want credential exit 3; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if strings.Contains(err.Error(), providerSelectionRequiredDiagnostic) {
		t.Fatalf("explicit Hetzner selection hit no-provider guard: %v", err)
	}
}

func TestBrokerProviderIsActionableUserSelection(t *testing.T) {
	clearConfigEnv(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("broker:\n  provider: aws\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", path)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "aws" || cfg.brokerProvider != "aws" || cfg.providerSelectionSource != providerSelectionUserConfig || !providerSelectionIsActionable(cfg) {
		t.Fatalf("broker provider selection=%#v source=%q", cfg.brokerProvider, cfg.providerSelectionSource)
	}
}

func TestUnconfiguredProviderCommandsUseSharedSelectionDiagnostic(t *testing.T) {
	commands := []struct {
		name string
		run  func(App) error
	}{
		{name: "warmup", run: func(app App) error { return app.warmup(context.Background(), nil) }},
		{name: "new run", run: func(app App) error {
			return app.runCommand(context.Background(), []string{"--no-sync", "--", "true"})
		}},
		{name: "list", run: func(app App) error { return app.list(context.Background(), nil) }},
		{name: "cleanup dry run", run: func(app App) error {
			return app.cleanup(context.Background(), []string{"--dry-run"})
		}},
		{name: "unknown status", run: func(app App) error {
			return app.status(context.Background(), []string{"--id", "unknown-provider-selection"})
		}},
		{name: "unknown inspect", run: func(app App) error {
			return app.inspect(context.Background(), []string{"--id", "unknown-provider-selection"})
		}},
		{name: "unknown stop", run: func(app App) error {
			return app.stop(context.Background(), []string{"--id", "unknown-provider-selection"})
		}},
	}

	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
			t.Setenv("CRABBOX_PROVIDER", "")
			t.Setenv("HCLOUD_TOKEN", "")
			t.Setenv("HETZNER_TOKEN", "")
			var stdout, stderr bytes.Buffer
			err := command.run(App{Stdout: &stdout, Stderr: &stderr})
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(err.Error(), providerSelectionRequiredDiagnostic) {
				t.Fatalf("error=%v, want shared exit 2 diagnostic; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			output := err.Error() + stdout.String() + stderr.String()
			if strings.Contains(output, "HCLOUD_TOKEN") || strings.Contains(output, "HETZNER_TOKEN") {
				t.Fatalf("command emitted Hetzner credential guidance: %s", output)
			}
		})
	}
}
