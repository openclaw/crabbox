package cli

import "testing"

func TestParallelsUserExplicitInputs(t *testing.T) {
	for _, source := range []providerSelectionSource{providerSelectionUserConfig, providerSelectionRepoConfig} {
		t.Run(string(source), func(t *testing.T) {
			for _, user := range []string{"", "crabbox", "guest-user"} {
				cfg := baseConfig()
				file := fileConfig{Parallels: &fileParallelsConfig{User: user}}
				if err := applyFileConfigWithTrustAndProviderSource(&cfg, file, source == providerSelectionUserConfig, source); err != nil {
					t.Fatal(err)
				}
				if cfg.Parallels.UserExplicit != (user != "") {
					t.Fatalf("user %q: explicit = %t", user, cfg.Parallels.UserExplicit)
				}
			}
		})
	}
	t.Run("environment", func(t *testing.T) {
		for _, user := range []string{"", "crabbox", "guest-user"} {
			t.Setenv("CRABBOX_PARALLELS_USER", user)
			cfg := baseConfig()
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Parallels.UserExplicit != (user != "") {
				t.Fatalf("user %q: explicit = %t", user, cfg.Parallels.UserExplicit)
			}
		}
	})
	t.Run("empty environment preserves file selection", func(t *testing.T) {
		t.Setenv("CRABBOX_PARALLELS_USER", "")
		cfg := baseConfig()
		if err := applyFileConfig(&cfg, fileConfig{Parallels: &fileParallelsConfig{User: "crabbox"}}); err != nil {
			t.Fatal(err)
		}
		if err := applyEnv(&cfg); err != nil {
			t.Fatal(err)
		}
		if !cfg.Parallels.UserExplicit || cfg.Parallels.User != "crabbox" {
			t.Fatalf("file user lost: %#v", cfg.Parallels)
		}
	})
}
