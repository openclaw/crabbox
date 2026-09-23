package cli

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	providerHistoryTestPrimary   = "history-provider-a"
	providerHistoryTestSecondary = "history-provider-b"
	providerHistoryTestAlias     = "history-provider-alias"
)

type providerHistoryTestProvider struct {
	name    string
	aliases []string
}

func (p providerHistoryTestProvider) Spec() ProviderSpec {
	return ProviderSpec{
		Name:    p.name,
		Aliases: append([]string(nil), p.aliases...),
		Kind:    ProviderKindSSHLease,
		Targets: []TargetSpec{{OS: targetLinux}},
	}
}

func (providerHistoryTestProvider) RegisterFlags(*flag.FlagSet, Config) any { return nil }
func (providerHistoryTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (providerHistoryTestProvider) Configure(Config, Runtime) (Backend, error) { return nil, nil }

var providerHistoryTestRoster = []providerHistoryTestProvider{
	{name: providerHistoryTestPrimary, aliases: []string{providerHistoryTestAlias}},
	{name: providerHistoryTestSecondary},
	{name: "history-provider-c"},
	{name: "history-provider-d"},
	{name: "history-provider-e"},
	{name: "history-provider-f"},
	{name: "history-provider-g"},
	{name: "history-provider-h"},
	{name: "history-provider-i"},
	{name: "history-provider-j"},
}

func setupProviderHistoryTest(t *testing.T) string {
	t.Helper()
	clearConfigEnv(t)
	for _, provider := range providerHistoryTestRoster {
		RegisterProvider(provider)
		provider := provider
		t.Cleanup(func() {
			delete(providerRegistry, normalizeProviderName(provider.name))
			for _, alias := range provider.aliases {
				delete(providerRegistry, normalizeProviderName(alias))
			}
		})
	}
	root := t.TempDir()
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
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

	if err := rememberProviderForCurrentWorkspace(providerHistoryTestPrimary, first); err != nil {
		t.Fatal(err)
	}
	if err := rememberProviderForCurrentWorkspace(providerHistoryTestSecondary, second); err != nil {
		t.Fatal(err)
	}
	if err := rememberProviderForCurrentWorkspace(providerHistoryTestPrimary, third); err != nil {
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
	if record.Providers[0].Provider != providerHistoryTestPrimary || !record.Providers[0].LastSelectedAt.Equal(third) {
		t.Fatalf("first provider=%#v", record.Providers[0])
	}
	if record.Providers[1].Provider != providerHistoryTestSecondary || !record.Providers[1].LastSelectedAt.Equal(second) {
		t.Fatalf("second provider=%#v", record.Providers[1])
	}
}

func TestProviderHistoryCanonicalizesAliasesAndCapsEntries(t *testing.T) {
	setupProviderHistoryTest(t)
	names := make([]string, 0, providerHistoryLimit+2)
	for _, provider := range registeredProviders() {
		switch provider.Spec().Kind {
		case ProviderKindSSHLease, ProviderKindDelegatedRun:
			names = append(names, provider.Spec().Name)
		}
		if len(names) == providerHistoryLimit+2 {
			break
		}
	}
	if len(names) < providerHistoryLimit+1 {
		t.Fatalf("need at least %d runnable providers, got %d", providerHistoryLimit+1, len(names))
	}
	start := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	for i, name := range names {
		if err := rememberProviderForCurrentWorkspace(name, start.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("remember %s: %v", name, err)
		}
	}
	record, ok, err := readProviderHistory()
	if err != nil || !ok {
		t.Fatalf("read history ok=%t err=%v", ok, err)
	}
	if len(record.Providers) != providerHistoryLimit {
		t.Fatalf("history len=%d want=%d", len(record.Providers), providerHistoryLimit)
	}
	if record.Providers[0].Provider != names[len(names)-1] {
		t.Fatalf("MRU provider=%q want=%q", record.Providers[0].Provider, names[len(names)-1])
	}

	// Provider aliases are persisted canonically.
	if err := rememberProviderForCurrentWorkspace(providerHistoryTestAlias, start.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	record, _, err = readProviderHistory()
	if err != nil {
		t.Fatal(err)
	}
	if record.Providers[0].Provider != providerHistoryTestPrimary {
		t.Fatalf("alias persisted as %q", record.Providers[0].Provider)
	}
}

func TestRecentProviderFallbackIsLowerPriorityThanConfig(t *testing.T) {
	setupProviderHistoryTest(t)
	if err := rememberProviderForCurrentWorkspace(providerHistoryTestPrimary, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig()
	applyRecentProviderFallback(&cfg)
	if cfg.Provider != providerHistoryTestPrimary || cfg.providerSelectionSource != providerSelectionRecentHistory {
		t.Fatalf("history fallback provider=%q source=%q", cfg.Provider, cfg.providerSelectionSource)
	}

	setProviderSelection(&cfg, providerHistoryTestSecondary, providerSelectionUserConfig)
	applyRecentProviderFallback(&cfg)
	if cfg.Provider != providerHistoryTestSecondary || cfg.providerSelectionSource != providerSelectionUserConfig {
		t.Fatalf("user config lost to history: provider=%q source=%q", cfg.Provider, cfg.providerSelectionSource)
	}
}

func TestRecentProviderFallbackDisabledInCI(t *testing.T) {
	setupProviderHistoryTest(t)
	if err := rememberProviderForCurrentWorkspace(providerHistoryTestPrimary, time.Now().UTC()); err != nil {
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
	setProviderSelection(&cfg, providerHistoryTestPrimary, providerSelectionRecentHistory)

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

func TestProviderHistoryClearIsIdempotentWhenStateDoesNotExist(t *testing.T) {
	setupProviderHistoryTest(t)
	root, removed, err := clearProviderHistoryForCurrentWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	if removed || root == "" {
		t.Fatalf("clear empty history root=%q removed=%t", root, removed)
	}
}

func TestRememberExplicitProviderRequiresRealFlagIntent(t *testing.T) {
	setupProviderHistoryTest(t)
	cfg := baseConfig()
	setProviderSelection(&cfg, providerHistoryTestPrimary, providerSelectionFlag)
	cfg.providerExplicit = false
	rememberExplicitProviderBestEffort(cfg, &bytes.Buffer{})
	if _, ok, err := readProviderHistory(); err != nil || ok {
		t.Fatalf("programmatic flag source wrote history ok=%t err=%v", ok, err)
	}

	cfg.providerExplicit = true
	rememberExplicitProviderBestEffort(cfg, &bytes.Buffer{})
	record, ok, err := readProviderHistory()
	if err != nil || !ok || len(record.Providers) != 1 || record.Providers[0].Provider != providerHistoryTestPrimary {
		t.Fatalf("explicit provider history=%#v ok=%t err=%v", record, ok, err)
	}
}

func TestProviderHistoryCommandShowsAndClearsCurrentWorkspace(t *testing.T) {
	setupProviderHistoryTest(t)
	if err := rememberProviderForCurrentWorkspace(providerHistoryTestPrimary, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}

	if err := app.providerHistory([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"provider":"`+providerHistoryTestPrimary+`"`) {
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
	if err := rememberProviderForCurrentWorkspace(providerHistoryTestPrimary, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != providerHistoryTestPrimary || cfg.providerSelectionSource != providerSelectionRecentHistory {
		t.Fatalf("loadConfig provider=%q source=%q", cfg.Provider, cfg.providerSelectionSource)
	}

	t.Setenv("CRABBOX_PROVIDER", providerHistoryTestSecondary)
	cfg, err = loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != providerHistoryTestSecondary || cfg.providerSelectionSource != providerSelectionEnvironment {
		t.Fatalf("env provider=%q source=%q", cfg.Provider, cfg.providerSelectionSource)
	}
}

func TestConfigShowReportsRecentProviderHistorySource(t *testing.T) {
	setupProviderHistoryTest(t)
	if err := rememberProviderForCurrentWorkspace(providerHistoryTestPrimary, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &stderr}).configShow([]string{"--json"}); err != nil {
		t.Fatalf("config show: %v stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"provider":"`+providerHistoryTestPrimary+`"`) ||
		!strings.Contains(stdout.String(), `"providerSource":"recent_history"`) ||
		!strings.Contains(stdout.String(), `"providerSelected":true`) {
		t.Fatalf("config show history provenance=%q", stdout.String())
	}
}

func TestProviderHistoryFallbackRequiresNoExplicitConfigFile(t *testing.T) {
	setupProviderHistoryTest(t)
	if err := rememberProviderForCurrentWorkspace(providerHistoryTestPrimary, time.Now().UTC()); err != nil {
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
