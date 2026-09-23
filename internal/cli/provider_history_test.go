package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupProviderHistoryTest(t *testing.T) string {
	t.Helper()
	clearConfigEnv(t)
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("CI", "")
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv(controllerProviderScopeEnv, "")
	t.Setenv(controllerWorkspaceIDEnv, "")
	return canonicalRepositoryPath(root)
}

func TestProviderHistoryMaintainsBoundedMRUOrder(t *testing.T) {
	root := setupProviderHistoryTest(t)
	first := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)
	third := second.Add(time.Minute)

	if err := rememberProviderForCurrentWorkspace("boxd", first); err != nil {
		t.Fatal(err)
	}
	if err := rememberProviderForCurrentWorkspace("aws", second); err != nil {
		t.Fatal(err)
	}
	if err := rememberProviderForCurrentWorkspace("boxd", third); err != nil {
		t.Fatal(err)
	}

	record, ok, err := readProviderHistoryForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("provider history missing")
	}
	if len(record.Providers) != 2 {
		t.Fatalf("providers=%#v", record.Providers)
	}
	if record.Providers[0].Provider != "boxd" || !record.Providers[0].LastSelectedAt.Equal(third) {
		t.Fatalf("first provider=%#v", record.Providers[0])
	}
	if record.Providers[1].Provider != "aws" || !record.Providers[1].LastSelectedAt.Equal(second) {
		t.Fatalf("second provider=%#v", record.Providers[1])
	}
}

func TestRecentProviderFallbackIsLowerPriorityThanConfig(t *testing.T) {
	setupProviderHistoryTest(t)
	if err := rememberProviderForCurrentWorkspace("boxd", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig()
	applyRecentProviderFallback(&cfg)
	if cfg.Provider != "boxd" || cfg.providerSelectionSource != providerSelectionRecentHistory {
		t.Fatalf("history fallback provider=%q source=%q", cfg.Provider, cfg.providerSelectionSource)
	}

	setProviderSelection(&cfg, "aws", providerSelectionUserConfig)
	applyRecentProviderFallback(&cfg)
	if cfg.Provider != "aws" || cfg.providerSelectionSource != providerSelectionUserConfig {
		t.Fatalf("user config lost to history: provider=%q source=%q", cfg.Provider, cfg.providerSelectionSource)
	}
}

func TestRecentProviderFallbackDisabledInCI(t *testing.T) {
	setupProviderHistoryTest(t)
	if err := rememberProviderForCurrentWorkspace("boxd", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CI", "1")

	cfg := baseConfig()
	beforeProvider := cfg.Provider
	beforeSource := cfg.providerSelectionSource
	applyRecentProviderFallback(&cfg)
	if cfg.Provider != beforeProvider || cfg.providerSelectionSource != beforeSource {
		t.Fatalf("CI consumed local provider history: provider=%q source=%q", cfg.Provider, cfg.providerSelectionSource)
	}
}

func TestRecentProviderDoesNotBlockLeaseIDRouting(t *testing.T) {
	setupProviderHistoryTest(t)
	cfg := baseConfig()
	setProviderSelection(&cfg, "boxd", providerSelectionRecentHistory)

	fs := newFlagSet("history-route-test", &bytes.Buffer{})
	registerProviderSelectionFlag(fs, cfg, providerHelpAll())
	if err := autoRouteStaticLease(&cfg, fs, "static_history-route"); err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != staticProvider || cfg.providerSelectionSource != providerSelectionLeaseContext {
		t.Fatalf("static route provider=%q source=%q", cfg.Provider, cfg.providerSelectionSource)
	}
}

func TestCorruptProviderHistoryIsIgnoredByNormalSelectionAndVisibleToInspection(t *testing.T) {
	root := setupProviderHistoryTest(t)
	path, err := providerHistoryPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig()
	beforeProvider := cfg.Provider
	beforeSource := cfg.providerSelectionSource
	applyRecentProviderFallback(&cfg)
	if cfg.Provider != beforeProvider || cfg.providerSelectionSource != beforeSource {
		t.Fatalf("corrupt history changed provider=%q source=%q", cfg.Provider, cfg.providerSelectionSource)
	}

	var stdout, stderr bytes.Buffer
	err = (App{Stdout: &stdout, Stderr: &stderr}).providerHistory(nil)
	if err == nil || !strings.Contains(err.Error(), "decode provider history state") {
		t.Fatalf("provider history inspection error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

func TestProviderHistoryCommandShowsAndClearsCurrentWorkspace(t *testing.T) {
	setupProviderHistoryTest(t)
	if err := rememberProviderForCurrentWorkspace("boxd", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}

	if err := app.providerHistory([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"provider":"boxd"`) {
		t.Fatalf("history json=%q", stdout.String())
	}

	stdout.Reset()
	if err := app.providerHistory([]string{"--clear"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "cleared provider history") {
		t.Fatalf("clear output=%q", stdout.String())
	}
	if _, ok, err := readProviderHistory(); err != nil || ok {
		t.Fatalf("history still present ok=%t err=%v", ok, err)
	}
}

func TestProviderHistoryLoadConfigIntegration(t *testing.T) {
	setupProviderHistoryTest(t)
	if err := rememberProviderForCurrentWorkspace("boxd", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "boxd" || cfg.providerSelectionSource != providerSelectionRecentHistory {
		t.Fatalf("loadConfig provider=%q source=%q", cfg.Provider, cfg.providerSelectionSource)
	}

	t.Setenv("CRABBOX_PROVIDER", "aws")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "aws" || cfg.providerSelectionSource != providerSelectionEnvironment {
		t.Fatalf("env provider=%q source=%q", cfg.Provider, cfg.providerSelectionSource)
	}
}

func TestProviderHistoryFallbackRequiresNoExplicitConfigFile(t *testing.T) {
	setupProviderHistoryTest(t)
	if err := rememberProviderForCurrentWorkspace("boxd", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", path)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.providerSelectionSource == providerSelectionRecentHistory {
		t.Fatalf("explicit config unexpectedly consumed provider history: %#v", cfg)
	}
}

func TestProviderHistoryCommandRejectsClearJSONCombination(t *testing.T) {
	setupProviderHistoryTest(t)
	err := (App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).providerHistory([]string{"--clear", "--json"})
	if err == nil {
		t.Fatal("expected usage error")
	}
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("error=%v", err)
	}
}

var _ = context.Background
