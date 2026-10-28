package cli

import "testing"

func TestParallelsUserExplicitInputs(t *testing.T) {
	for _, source := range []providerSelectionSource{providerSelectionUserConfig, providerSelectionRepoConfig} {
		t.Run(string(source), func(t *testing.T) {
			clearConfigEnv(t)
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
		clearConfigEnv(t)
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
		clearConfigEnv(t)
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

func TestParallelsUserDoesNotBecomeGenericSSHUser(t *testing.T) {
	for _, genericUser := range []string{"", "generic-user"} {
		t.Run(genericUser, func(t *testing.T) {
			clearConfigEnv(t)
			cfg := baseConfig()
			file := fileConfig{Parallels: &fileParallelsConfig{User: "guest-user"}, SSH: &fileSSHConfig{User: genericUser}}
			if err := applyFileConfig(&cfg, file); err != nil {
				t.Fatal(err)
			}
			cfg.Provider = parallelsProvider
			if err := applyProviderConfigDefaults(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.SSHUser != "guest-user" || IsSSHUserExplicit(&cfg) != (genericUser != "") {
				t.Fatalf("Parallels user = %q, generic explicit = %t", cfg.SSHUser, IsSSHUserExplicit(&cfg))
			}
			cfg.Provider = "hetzner"
			if err := applyProviderConfigDefaults(&cfg); err != nil {
				t.Fatal(err)
			}
			want := genericUser
			if want == "" {
				want = baseConfig().SSHUser
			}
			if cfg.SSHUser != want {
				t.Fatalf("user after provider switch = %q, want %q", cfg.SSHUser, want)
			}
		})
	}
}
