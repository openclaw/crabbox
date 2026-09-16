package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGitSeedSourceConfigLayers(t *testing.T) {
	clearConfigEnv(t)
	cfg := baseConfig()
	if cfg.Sync.GitSeedSource != "origin" {
		t.Fatalf("default git seed source=%q", cfg.Sync.GitSeedSource)
	}
	var file fileConfig
	if err := yaml.Unmarshal([]byte("sync: {gitSeedSource: local}\n"), &file); err != nil {
		t.Fatal(err)
	}
	if err := applyFileConfig(&cfg, file); err != nil {
		t.Fatal(err)
	}
	if cfg.Sync.GitSeedSource != "local" {
		t.Fatalf("YAML git seed source=%q", cfg.Sync.GitSeedSource)
	}
	t.Setenv("CRABBOX_SYNC_GIT_SEED_SOURCE", "origin")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Sync.GitSeedSource != "origin" {
		t.Fatalf("environment did not override YAML: %q", cfg.Sync.GitSeedSource)
	}
	t.Setenv("CRABBOX_SYNC_GIT_SEED_SOURCE", "unsupported")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal("inactive sync configuration blocked loading:", err)
	}
	if err := validateGitSeedSource(cfg); err == nil {
		t.Fatal("active invalid source accepted")
	}
}

func TestGitSeedSourceConfigInputAttribution(t *testing.T) {
	for _, tc := range []struct {
		name, source, value string
	}{
		{"user local", "user_config", "local"},
		{"repo local", "repo_config", "local"},
		{"environment local", "environment", "local"},
		{"user explicit default", "user_config", "origin"},
		{"repo explicit default", "repo_config", "origin"},
		{"environment explicit default", "environment", "origin"},
		{"user empty", "user_config", ""},
		{"repo empty", "repo_config", ""},
		{"environment empty", "environment", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			if tc.source == "environment" {
				t.Setenv("CRABBOX_SYNC_GIT_SEED_SOURCE", tc.value)
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
			} else {
				file := fileConfig{Sync: &fileSyncConfig{GitSeedSource: tc.value}}
				source := providerSelectionUserConfig
				if tc.source == "repo_config" {
					source = providerSelectionRepoConfig
				}
				if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, source == providerSelectionUserConfig, source); err != nil {
					t.Fatal(err)
				}
			}
			got := cfg.inputProvenance.summary(configInputGeneric)
			if tc.value == "" {
				if len(cfg.inputProvenance) != 0 || cfg.Sync.GitSeedSource != "origin" {
					t.Fatalf("empty input changed configuration: source=%q provenance=%#v", cfg.Sync.GitSeedSource, cfg.inputProvenance)
				}
				return
			}
			if cfg.Sync.GitSeedSource != tc.value || got.state != "present" || !reflect.DeepEqual(got.sources, []string{tc.source}) || got.effects != configInputValue || got.complete || len(cfg.inputProvenance) != 1 {
				t.Fatalf("source=%q provenance=%#v", cfg.Sync.GitSeedSource, got)
			}
		})
	}
}

func TestGitSeedSourceConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync SyncConfig
		want string
	}{
		{"programmatic default", SyncConfig{}, ""},
		{"origin seed disabled", SyncConfig{GitSeedSource: "origin"}, ""},
		{"origin overlay", SyncConfig{GitSeedSource: "origin", GitSeed: true, GitOverlay: true}, ""},
		{"origin directory", SyncConfig{GitSeedSource: "origin", Source: "directory"}, ""},
		{"local default Git source", SyncConfig{GitSeedSource: "local", GitSeed: true}, ""},
		{"local Git source", SyncConfig{GitSeedSource: "local", GitSeed: true, Source: "git"}, ""},
		{"local whitespace", SyncConfig{GitSeedSource: " local ", GitSeed: true}, ""},
		{"invalid source", SyncConfig{GitSeedSource: "remote"}, "must be origin or local"},
		{"local seed disabled", SyncConfig{GitSeedSource: "local"}, "requires sync.gitSeed=true"},
		{"local directory", SyncConfig{GitSeedSource: "local", GitSeed: true, Source: "directory"}, "requires sync.source=git"},
		{"local overlay", SyncConfig{GitSeedSource: "local", GitSeed: true, GitOverlay: true}, "both own repository metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGitSeedSource(Config{Sync: tc.sync})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || ExitCodeForError(err, 1) != 2 {
				t.Fatalf("error=%v, want config error containing %q", err, tc.want)
			}
		})
	}
}

func TestGitSeedSourceConfigShow(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"", "origin"}, {"  ", "origin"}, {"origin", "origin"}, {"local", "local"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Sync.GitSeedSource = tc.value
			data, err := json.Marshal(configShowView(cfg))
			if err != nil {
				t.Fatal(err)
			}
			var shown struct {
				Sync struct{ GitSeedSource string }
			}
			if err := json.Unmarshal(data, &shown); err != nil {
				t.Fatal(err)
			}
			if shown.Sync.GitSeedSource != tc.want {
				t.Fatalf("JSON source=%q, want %q", shown.Sync.GitSeedSource, tc.want)
			}
			var text bytes.Buffer
			if err := writeConfigShowText(&text, cfg); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(text.String(), "git_seed_source="+tc.want) {
				t.Fatal("source missing from text configuration")
			}
		})
	}
}
