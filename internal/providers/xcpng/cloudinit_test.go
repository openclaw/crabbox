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

func TestCloudInitPayloadUsesRetryingPackageBootstrap(t *testing.T) {
	payload, err := newCloudInitPayload(testConfig(), "cbx_lease", "blue", "ssh-ed25519 AAAATEST crabbox")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"package_update: false",
		"package_upgrade: false",
		"bash -euxo pipefail <<'BOOT'",
		"Acquire::Retries \"8\";",
		"Acquire::http::Timeout \"30\";",
		"Acquire::https::Timeout \"30\";",
		"retry apt-get update",
		"retry apt-get install -y --no-install-recommends openssh-server ca-certificates curl git rsync jq",
	} {
		if !strings.Contains(payload.UserData, want) {
			t.Fatalf("user-data missing %q:\n%s", want, payload.UserData)
		}
	}
	if strings.Contains(payload.UserData, "\npackages:\n") {
		t.Fatalf("cloud-init payload must not use one-shot packages module:\n%s", payload.UserData)
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

func TestBuildConfigDriveImageRejectsOversizedPayload(t *testing.T) {
	payload := xcpNgCloudInitPayload{
		UserData: strings.Repeat("u", 11<<20),
		MetaData: "instance-id: cbx_lease\n",
	}
	if _, err := buildConfigDriveImage(payload); err == nil || !strings.Contains(err.Error(), "config-drive payload is too large") {
		t.Fatalf("err=%v, want oversized payload validation", err)
	}
}

func TestLinuxAutoinstallPayloadIncludesUbuntuServerContract(t *testing.T) {
	payload, err := newLinuxAutoinstallPayload(testConfig(), "cbx_lease", "blue", "ssh-ed25519 AAAATEST crabbox")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#cloud-config",
		"autoinstall:",
		"version: 1",
		"id: ubuntu-server",
		"name: direct",
		"install-server: true",
		"ssh_authorized_keys",
		"xe-guest-utilities",
		"/usr/local/bin/crabbox-ready",
		"systemctl enable xe-linux-distribution || true",
		"touch, /var/lib/crabbox/bootstrapped",
		"shutdown: reboot",
	} {
		if !strings.Contains(payload.UserData, want) {
			t.Fatalf("autoinstall user-data missing %q:\n%s", want, payload.UserData)
		}
	}
	if !strings.Contains(payload.MetaData, "instance-id: cbx_lease") || !strings.Contains(payload.MetaData, "local-hostname: crabbox-blue") {
		t.Fatalf("meta-data=%q", payload.MetaData)
	}
}

func TestLinuxAutoinstallPayloadRejectsMissingUserOrKey(t *testing.T) {
	cfg := testConfig()
	cfg.XCPNg.User = ""
	cfg.SSHUser = ""
	if _, err := newLinuxAutoinstallPayload(cfg, "cbx_lease", "blue", "ssh-ed25519 AAAATEST crabbox"); err == nil {
		t.Fatal("expected missing user error")
	}
	if _, err := newLinuxAutoinstallPayload(testConfig(), "cbx_lease", "blue", ""); err == nil {
		t.Fatal("expected missing public key error")
	}
}

func TestWindowsAutounattendPayloadIncludesBootstrapScriptAndAutoLogon(t *testing.T) {
	cfg := testConfig()
	cfg.XCPNg.Password = "credential-value"
	cfg.WindowsMode = "wsl2"
	cfg.WorkRoot = "/work/crabbox"
	payload, err := newWindowsAutounattendPayload(cfg, "cbx_lease", "blue", "ssh-ed25519 AAAATEST crabbox", "TempPass1!")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<unattend xmlns="urn:schemas-microsoft-com:unattend"`,
		`<AutoLogon>`,
		`<Username>crabbox</Username>`,
		`Crabbox Windows ISO E2E`,
		`<Key>/IMAGE/INDEX</Key><Value>1</Value>`,
		`powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand`,
		`<ComputerName>CRABBOX-BLUE</ComputerName>`,
	} {
		if !strings.Contains(payload.AnswerXML, want) {
			t.Fatalf("answer xml missing %q:\n%s", want, payload.AnswerXML)
		}
	}
	for _, want := range []string{
		`OpenSSH-Win64.zip`,
		`install-sshd.ps1`,
		`$workRoot = 'C:\crabbox'`,
		`ssh-ed25519 AAAATEST crabbox`,
	} {
		if !strings.Contains(payload.BootstrapPowerShell, want) {
			t.Fatalf("bootstrap powershell missing %q:\n%s", want, payload.BootstrapPowerShell)
		}
	}
	if payload.Username != "crabbox" {
		t.Fatalf("username=%q", payload.Username)
	}
	if strings.Contains(payload.AnswerXML, cfg.XCPNg.Password) || strings.Contains(payload.BootstrapPowerShell, cfg.XCPNg.Password) {
		t.Fatal("windows unattended payload leaked XCP-ng API password")
	}
}

func TestWindowsAutounattendPayloadRejectsMissingUserKeyOrPassword(t *testing.T) {
	cfg := testConfig()
	cfg.XCPNg.User = ""
	cfg.SSHUser = ""
	if _, err := newWindowsAutounattendPayload(cfg, "cbx_lease", "blue", "ssh-ed25519 AAAATEST crabbox", "TempPass1!"); err == nil {
		t.Fatal("expected missing user error")
	}
	if _, err := newWindowsAutounattendPayload(testConfig(), "cbx_lease", "blue", "", "TempPass1!"); err == nil {
		t.Fatal("expected missing public key error")
	}
	if _, err := newWindowsAutounattendPayload(testConfig(), "cbx_lease", "blue", "ssh-ed25519 AAAATEST crabbox", ""); err == nil {
		t.Fatal("expected missing password error")
	}
}

func TestUbuntuAutoinstallLinuxLinePatternAddsFlagAcrossSpacingVariants(t *testing.T) {
	input := strings.Join([]string{
		`menuentry "Try or Install Ubuntu Server" {`,
		`    linux  /casper/vmlinuz  --- `,
		`}`,
		`menuentry "Already Has Args" {`,
		`linux /casper/vmlinuz quiet splash ---`,
		`}`,
	}, "\n")
	updated := isoE2EUbuntuLinuxLinePattern.ReplaceAllStringFunc(input, func(line string) string {
		parts := isoE2EUbuntuLinuxLinePattern.FindStringSubmatch(line)
		if len(parts) != 4 {
			return line
		}
		middle := strings.TrimSpace(parts[2])
		if middle == "" {
			middle = "autoinstall"
		} else {
			middle = middle + " autoinstall"
		}
		return parts[1] + middle + parts[3]
	})
	for _, want := range []string{
		`linux  /casper/vmlinuz autoinstall ---`,
		`linux /casper/vmlinuz quiet splash autoinstall ---`,
	} {
		if !strings.Contains(updated, want) {
			t.Fatalf("updated grub missing %q:\n%s", want, updated)
		}
	}
}
