package xcpng

import (
	"strings"
	"testing"
)

func TestCloudInitPayloadIncludesSSHUserKeyAndBootstrap(t *testing.T) {
	cfg := testConfig()
	cfg.XCPNg.Password = "credential-value"
	payload, err := newCloudInitPayload(cfg, "cbx_lease", "blue", "ssh-ed25519 AAAATEST crabbox")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#cloud-config",
		"name: crabbox",
		"ssh-ed25519 AAAATEST crabbox",
		"NOPASSWD",
		"openssh-server",
		"jq",
		"/work/crabbox",
		"/usr/local/bin/crabbox-ready",
		"test -f /var/lib/crabbox/bootstrapped",
		"test -w '/work/crabbox'",
		"/var/lib/crabbox/bootstrapped",
		"/var/cache/crabbox/pnpm",
	} {
		if !strings.Contains(payload.UserData, want) {
			t.Fatalf("user-data missing %q:\n%s", want, payload.UserData)
		}
	}
	if !strings.Contains(payload.MetaData, "instance-id: cbx_lease") || !strings.Contains(payload.MetaData, "local-hostname: crabbox-blue") {
		t.Fatalf("meta-data=%q", payload.MetaData)
	}
	if strings.Contains(payload.UserData, cfg.XCPNg.Password) || strings.Contains(payload.MetaData, cfg.XCPNg.Password) {
		t.Fatal("cloud-init payload leaked XCP-ng API password")
	}
}

func TestCloudInitPayloadConfiguresSSHPortContract(t *testing.T) {
	cfg := testConfig()
	cfg.SSHPort = "2222"
	cfg.SSHFallbackPorts = []string{"22"}
	payload, err := newCloudInitPayload(cfg, "cbx_lease", "blue", "ssh-ed25519 AAAATEST crabbox")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/etc/ssh/sshd_config.d/99-crabbox-port.conf",
		"Port 2222",
		"Port 22",
		"PasswordAuthentication no",
		"systemctl enable ssh || true",
		"timeout 30s systemctl restart ssh || timeout 30s systemctl restart ssh.socket || true",
	} {
		if !strings.Contains(payload.UserData, want) {
			t.Fatalf("user-data missing %q:\n%s", want, payload.UserData)
		}
	}
	if strings.Contains(payload.UserData, "systemctl, enable, --now, ssh") {
		t.Fatalf("user-data still uses blocking ssh enable command:\n%s", payload.UserData)
	}
}

func TestCloudInitPayloadQuotesWorkRootInRunCommands(t *testing.T) {
	cfg := testConfig()
	cfg.XCPNg.WorkRoot = "/work/crabbox,with-comma"
	payload, err := newCloudInitPayload(cfg, "cbx_lease", "blue", "ssh-ed25519 AAAATEST crabbox")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  - [mkdir, -p, '/work/crabbox,with-comma', /var/cache/crabbox/pnpm, /var/cache/crabbox/npm, /var/lib/crabbox]",
		"  - [chown, -R, 'crabbox:crabbox', '/work/crabbox,with-comma', /var/cache/crabbox]",
		"test -w '/work/crabbox,with-comma'",
	} {
		if !strings.Contains(payload.UserData, want) {
			t.Fatalf("user-data missing %q:\n%s", want, payload.UserData)
		}
	}
}

func TestCloudInitPayloadRejectsMissingUserOrKey(t *testing.T) {
	cfg := testConfig()
	cfg.XCPNg.User = ""
	cfg.SSHUser = ""
	if _, err := newCloudInitPayload(cfg, "cbx_lease", "blue", "ssh-ed25519 AAAATEST crabbox"); err == nil {
		t.Fatal("expected missing user error")
	}
	cfg.XCPNg.User = "crabbox"
	if _, err := newCloudInitPayload(cfg, "cbx_lease", "blue", ""); err == nil {
		t.Fatal("expected missing public key error")
	}
}

func TestConfigDriveLabelsRequireCrabboxOwnership(t *testing.T) {
	labels := configDriveLabels(map[string]string{
		"crabbox":    "true",
		"created_by": "crabbox",
		"provider":   "xcp-ng",
		"lease":      "cbx_lease",
	})
	if labels["resource"] != "config-drive" || labels["cleanup_with_vm"] != "true" || labels["lease"] != "cbx_lease" {
		t.Fatalf("labels=%#v", labels)
	}
}

func TestBuildConfigDriveImageContainsNoCloudFilesAndLabel(t *testing.T) {
	image, err := buildConfigDriveImage(xcpNgCloudInitPayload{UserData: "#cloud-config\n", MetaData: "instance-id: cbx_lease\n"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(image)
	for _, want := range []string{"CIDATA", "#cloud-config", "instance-id: cbx_lease"} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
			t.Fatalf("config-drive image missing %q", want)
		}
	}
	if !strings.Contains(text, "CRAB0001TXT") || !strings.Contains(text, "CRAB0002TXT") {
		t.Fatal("config-drive image missing file directory aliases")
	}
}
