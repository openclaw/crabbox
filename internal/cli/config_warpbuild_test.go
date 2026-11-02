package cli

import "testing"

func TestWarpBuildConfigurationTrustAndNativeToken(t *testing.T) {
	cfg := defaultWarpBuildConfig()
	_, err := cfg.applyFile(&fileWarpBuildConfig{CLIPath: "/untrusted/wb", Workdir: "/Users/runner/project"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CLIPath != "wb" || cfg.Workdir != "/Users/runner/project" {
		t.Fatalf("repository configuration crossed CLI trust boundary: %#v", cfg)
	}
	_, err = cfg.applyFile(&fileWarpBuildConfig{CLIPath: "/trusted/wb"}, true)
	if err != nil || cfg.CLIPath != "/trusted/wb" {
		t.Fatalf("trusted CLI override failed: %v", err)
	}
	t.Setenv("WB_TOKEN", "fixture-native-token")
	t.Setenv("CRABBOX_WARPBUILD_TOKEN", "fixture-selected-token")
	_, err = cfg.applyEnv()
	if err != nil || cfg.Token != "fixture-selected-token" {
		t.Fatalf("explicit token precedence failed: %v", err)
	}
}
