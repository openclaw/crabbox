package cli

import (
	"os"
	"strings"
	"testing"
)

func TestProxmoxTemplateCapabilitiesTrustedConfig(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		cfg := baseConfig()
		id := 9400
		file := &fileProxmoxConfig{DesktopTemplateID: &id, BrowserTemplateID: &id}
		source := configInputRepo
		if trusted {
			source = configInputUser
		}
		if err := applyProxmoxFileConfig(&cfg, file, source, credentialSourceTrustedFile); err != nil {
			t.Fatal(err)
		}
		want := 0
		if trusted {
			want = id
		}
		if cfg.Proxmox.DesktopTemplateID != want || cfg.Proxmox.BrowserTemplateID != want {
			t.Fatalf("trusted=%v config=%+v", trusted, cfg.Proxmox)
		}
	}
	cfg := baseConfig()
	t.Setenv("CRABBOX_PROXMOX_DESKTOP_TEMPLATE_ID", "9400")
	t.Setenv("CRABBOX_PROXMOX_BROWSER_TEMPLATE_ID", "9400")
	if err := applyProxmoxEnvironmentConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Proxmox.DesktopTemplateID != 9400 || cfg.Proxmox.BrowserTemplateID != 9400 {
		t.Fatal("environment opt-in ignored")
	}
	zero := 0
	if _, err := cfg.Proxmox.applyFile(&fileProxmoxConfig{DesktopTemplateID: &zero, BrowserTemplateID: &zero}, true); err != nil {
		t.Fatal(err)
	}
	if cfg.Proxmox.DesktopTemplateID != 0 || cfg.Proxmox.BrowserTemplateID != 0 {
		t.Fatal("explicit zero did not disable template capabilities")
	}
}

func TestProxmoxBootstrapChecksPrecedeReady(t *testing.T) {
	cfg := baseConfig()
	script := proxmoxBootstrapScriptWithChecks(cfg, "verify-prepared-template-or-fail", "check-running-desktop-or-fail")
	if strings.Index(script, "verify-prepared-template-or-fail") > strings.Index(script, "cat >/usr/local/bin/crabbox-ready") {
		t.Fatal("publishes readiness before template setup")
	}
	if strings.Index(script, "check-running-desktop-or-fail") > strings.Index(script, "\nREADY\n") {
		t.Fatal("desktop check missing from persistent readiness script")
	}
	if proxmoxBootstrapScript(cfg) != proxmoxBootstrapScriptWithChecks(cfg, "", "") {
		t.Fatal("headless bootstrap changed")
	}
}

func TestProxmoxBootstrapExtensionDelivered(t *testing.T) {
	cfg, path := installProxmoxBootstrapChild(t, "", 0)
	client := &ProxmoxClient{GuestBootstrap: func(Config) (string, string) { return "template-setup", "template-check" }}
	if err := client.bootstrapSSH(t.Context(), "fixture.invalid", cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != proxmoxBootstrapScriptWithChecks(cfg, "template-setup", "template-check") {
		t.Fatalf("extension input: %v", err)
	}
}
