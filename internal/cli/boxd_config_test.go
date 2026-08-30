package cli

import "testing"

func TestBoxdConfigDefaults(t *testing.T) {
	cfg := baseConfig()
	if cfg.Boxd.APIURL != "https://app.boxd.sh" || cfg.Boxd.WorkRoot != "/home/boxd/crabbox" || !cfg.Boxd.DeleteOnRelease {
		t.Fatalf("boxd defaults=%#v", cfg.Boxd)
	}
}

func TestBoxdUntrustedConfigCannotRedirectCredentialedAPI(t *testing.T) {
	cfg := baseConfig()
	cfg.Boxd.APIURL = "https://trusted.example.test"
	cfg.Boxd.Org = "trusted-org"
	deleteOnRelease := false
	file := fileConfig{Boxd: &fileBoxdConfig{
		APIURL:          "https://repository.example.test",
		Org:             "repository-org",
		WorkRoot:        "/home/boxd/project",
		DeleteOnRelease: &deleteOnRelease,
	}}
	if err := applyFileConfigWithTrust(&cfg, file, false); err != nil {
		t.Fatal(err)
	}
	if cfg.Boxd.APIURL != "https://trusted.example.test" || cfg.Boxd.Org != "trusted-org" {
		t.Fatalf("untrusted config redirected the credentialed API: %#v", cfg.Boxd)
	}
	if cfg.Boxd.WorkRoot != "/home/boxd/project" || cfg.Boxd.DeleteOnRelease {
		t.Fatalf("safe repository settings were not applied: %#v", cfg.Boxd)
	}
	if !DeleteOnReleaseExplicit(cfg, "boxd") || !IsBoxdWorkRootExplicit(&cfg) {
		t.Fatal("explicit repository release policy was not recorded")
	}
}

func TestBoxdTrustedConfigCanSelectOriginAndAccount(t *testing.T) {
	cfg := baseConfig()
	file := fileConfig{Boxd: &fileBoxdConfig{
		APIURL:   "https://trusted.example.test",
		Org:      "trusted-org",
		WorkRoot: "/home/boxd/project",
	}}
	if err := applyFileConfigWithTrust(&cfg, file, true); err != nil {
		t.Fatal(err)
	}
	if cfg.Boxd.APIURL != "https://trusted.example.test" || cfg.Boxd.Org != "trusted-org" || cfg.Boxd.WorkRoot != "/home/boxd/project" {
		t.Fatalf("trusted config was not applied: %#v", cfg.Boxd)
	}
}

func TestBoxdEnvironmentConfig(t *testing.T) {
	cfg := baseConfig()
	t.Setenv("CRABBOX_BOXD_API_URL", "https://environment.example.test")
	t.Setenv("CRABBOX_BOXD_ORG", "environment-org")
	t.Setenv("CRABBOX_BOXD_WORK_ROOT", "/home/boxd/environment")
	t.Setenv("CRABBOX_BOXD_DELETE_ON_RELEASE", "false")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Boxd.APIURL != "https://environment.example.test" || cfg.Boxd.Org != "environment-org" ||
		cfg.Boxd.WorkRoot != "/home/boxd/environment" || cfg.Boxd.DeleteOnRelease || !DeleteOnReleaseExplicit(cfg, "boxd") || !IsBoxdWorkRootExplicit(&cfg) {
		t.Fatalf("environment config was not applied: %#v", cfg.Boxd)
	}
}

func TestBoxdEmptyEnvironmentClearsTrustedAccountContext(t *testing.T) {
	cfg := baseConfig()
	cfg.Boxd.APIURL = "https://trusted.example.test"
	cfg.Boxd.Org = "trusted-org"
	t.Setenv("CRABBOX_BOXD_API_URL", "")
	t.Setenv("CRABBOX_BOXD_ORG", "")
	if err := applyEnv(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Boxd.APIURL != "" || cfg.Boxd.Org != "" {
		t.Fatalf("explicit empty environment did not clear trusted vendor context: %#v", cfg.Boxd)
	}
}
