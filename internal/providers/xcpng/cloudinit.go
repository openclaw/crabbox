package xcpng

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type xcpNgCloudInitPayload struct {
	UserData string
	MetaData string
}

type xcpNgLinuxAutoinstallPayload struct {
	UserData string
	MetaData string
}

type xcpNgWindowsAutounattendPayload struct {
	AnswerXML           string
	BootstrapPowerShell string
	Username            string
}

func newCloudInitPayload(cfg Config, leaseID, slug, publicKey string) (xcpNgCloudInitPayload, error) {
	user := strings.TrimSpace(core.Blank(cfg.XCPNg.User, cfg.SSHUser))
	if user == "" {
		return xcpNgCloudInitPayload{}, exit(2, "xcp-ng cloud-init user is required")
	}
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return xcpNgCloudInitPayload{}, exit(2, "xcp-ng cloud-init public key is required")
	}
	workRoot := core.Blank(cfg.XCPNg.WorkRoot, cfg.WorkRoot)
	var userData bytes.Buffer
	fmt.Fprintf(&userData, "#cloud-config\n")
	fmt.Fprintf(&userData, "users:\n")
	fmt.Fprintf(&userData, "  - name: %s\n", yamlSingleQuotedScalar(user))
	fmt.Fprintf(&userData, "    groups: [sudo]\n")
	fmt.Fprintf(&userData, "    shell: /bin/bash\n")
	fmt.Fprintf(&userData, "    sudo: ['ALL=(ALL) NOPASSWD:ALL']\n")
	fmt.Fprintf(&userData, "    ssh_authorized_keys:\n")
	fmt.Fprintf(&userData, "      - %s\n", shellSafeCloudInitScalar(publicKey))
	fmt.Fprintf(&userData, "package_update: false\n")
	fmt.Fprintf(&userData, "package_upgrade: false\n")
	fmt.Fprintf(&userData, "write_files:\n")
	fmt.Fprintf(&userData, "  - path: /etc/ssh/sshd_config.d/99-crabbox-port.conf\n")
	fmt.Fprintf(&userData, "    permissions: '0644'\n")
	fmt.Fprintf(&userData, "    content: |\n")
	fmt.Fprintf(&userData, "%s", cloudInitSSHPortConfig(cfg))
	fmt.Fprintf(&userData, "      PasswordAuthentication no\n")
	fmt.Fprintf(&userData, "  - path: /usr/local/bin/crabbox-ready\n")
	fmt.Fprintf(&userData, "    permissions: '0755'\n")
	fmt.Fprintf(&userData, "    content: |\n")
	fmt.Fprintf(&userData, "%s", indentMultiline(cloudInitReadyScript(workRoot), "      "))
	fmt.Fprintf(&userData, "runcmd:\n")
	fmt.Fprintf(&userData, "  - |\n")
	fmt.Fprintf(&userData, "    bash -euxo pipefail <<'BOOT'\n")
	fmt.Fprintf(&userData, "    export DEBIAN_FRONTEND=noninteractive\n")
	fmt.Fprintf(&userData, "    cat >/etc/apt/apt.conf.d/80-crabbox-retries <<'APT'\n")
	fmt.Fprintf(&userData, "    Acquire::Retries \"8\";\n")
	fmt.Fprintf(&userData, "    Acquire::http::Timeout \"30\";\n")
	fmt.Fprintf(&userData, "    Acquire::https::Timeout \"30\";\n")
	fmt.Fprintf(&userData, "    APT\n")
	fmt.Fprintf(&userData, "    retry() {\n")
	fmt.Fprintf(&userData, "      n=1\n")
	fmt.Fprintf(&userData, "      until \"$@\"; do\n")
	fmt.Fprintf(&userData, "        if [ \"$n\" -ge 8 ]; then\n")
	fmt.Fprintf(&userData, "          return 1\n")
	fmt.Fprintf(&userData, "        fi\n")
	fmt.Fprintf(&userData, "        sleep $((n * 5))\n")
	fmt.Fprintf(&userData, "        n=$((n + 1))\n")
	fmt.Fprintf(&userData, "      done\n")
	fmt.Fprintf(&userData, "    }\n")
	fmt.Fprintf(&userData, "    retry apt-get update\n")
	fmt.Fprintf(&userData, "    retry apt-get install -y --no-install-recommends openssh-server ca-certificates curl git rsync jq\n")
	fmt.Fprintf(&userData, "    BOOT\n")
	fmt.Fprintf(&userData, "  - [mkdir, -p, %s, /var/cache/crabbox/pnpm, /var/cache/crabbox/npm, /var/lib/crabbox]\n", yamlSingleQuotedScalar(workRoot))
	fmt.Fprintf(&userData, "  - [chown, -R, %s, %s, /var/cache/crabbox]\n", yamlSingleQuotedScalar(user+":"+user), yamlSingleQuotedScalar(workRoot))
	fmt.Fprintf(&userData, "  - |\n")
	fmt.Fprintf(&userData, "    systemctl enable ssh || true\n")
	fmt.Fprintf(&userData, "    systemctl disable --now ssh.socket || true\n")
	fmt.Fprintf(&userData, "    timeout 30s systemctl restart ssh.service || timeout 30s systemctl restart ssh || true\n")
	fmt.Fprintf(&userData, "  - [touch, /var/lib/crabbox/bootstrapped]\n")
	fmt.Fprintf(&userData, "  - [/usr/local/bin/crabbox-ready]\n")
	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: crabbox-%s\n", shellSafeCloudInitScalar(leaseID), shellSafeCloudInitScalar(slug))
	return xcpNgCloudInitPayload{UserData: userData.String(), MetaData: metaData}, nil
}

func newLinuxAutoinstallPayload(cfg Config, leaseID, slug, publicKey string) (xcpNgLinuxAutoinstallPayload, error) {
	user := strings.TrimSpace(core.Blank(cfg.XCPNg.User, cfg.SSHUser))
	if user == "" {
		return xcpNgLinuxAutoinstallPayload{}, exit(2, "xcp-ng linux autoinstall user is required")
	}
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return xcpNgLinuxAutoinstallPayload{}, exit(2, "xcp-ng linux autoinstall public key is required")
	}
	workRoot := core.Blank(cfg.XCPNg.WorkRoot, cfg.WorkRoot)
	var userData bytes.Buffer
	fmt.Fprintf(&userData, "#cloud-config\n")
	fmt.Fprintf(&userData, "autoinstall:\n")
	fmt.Fprintf(&userData, "  version: 1\n")
	fmt.Fprintf(&userData, "  source:\n")
	fmt.Fprintf(&userData, "    id: ubuntu-server\n")
	fmt.Fprintf(&userData, "  storage:\n")
	fmt.Fprintf(&userData, "    layout:\n")
	fmt.Fprintf(&userData, "      name: direct\n")
	fmt.Fprintf(&userData, "  ssh:\n")
	fmt.Fprintf(&userData, "    install-server: true\n")
	fmt.Fprintf(&userData, "    allow-pw: false\n")
	fmt.Fprintf(&userData, "  packages:\n")
	for _, pkg := range []string{"ca-certificates", "curl", "git", "jq", "rsync", "xe-guest-utilities"} {
		fmt.Fprintf(&userData, "    - %s\n", pkg)
	}
	fmt.Fprintf(&userData, "  user-data:\n")
	fmt.Fprintf(&userData, "    package_update: false\n")
	fmt.Fprintf(&userData, "    package_upgrade: false\n")
	fmt.Fprintf(&userData, "    users:\n")
	fmt.Fprintf(&userData, "      - default\n")
	fmt.Fprintf(&userData, "      - name: %s\n", yamlSingleQuotedScalar(user))
	fmt.Fprintf(&userData, "        gecos: Crabbox ISO E2E\n")
	fmt.Fprintf(&userData, "        groups: [adm, sudo]\n")
	fmt.Fprintf(&userData, "        shell: /bin/bash\n")
	fmt.Fprintf(&userData, "        sudo: ['ALL=(ALL) NOPASSWD:ALL']\n")
	fmt.Fprintf(&userData, "        lock_passwd: true\n")
	fmt.Fprintf(&userData, "        ssh_authorized_keys:\n")
	fmt.Fprintf(&userData, "          - %s\n", shellSafeCloudInitScalar(publicKey))
	fmt.Fprintf(&userData, "    write_files:\n")
	fmt.Fprintf(&userData, "      - path: /etc/ssh/sshd_config.d/99-crabbox-port.conf\n")
	fmt.Fprintf(&userData, "        permissions: '0644'\n")
	fmt.Fprintf(&userData, "        content: |\n")
	fmt.Fprintf(&userData, "%s", indentMultiline(strings.TrimRight(cloudInitSSHPortConfig(cfg), "\n")+"\n      PasswordAuthentication no", "          "))
	fmt.Fprintf(&userData, "      - path: /usr/local/bin/crabbox-ready\n")
	fmt.Fprintf(&userData, "        permissions: '0755'\n")
	fmt.Fprintf(&userData, "        content: |\n")
	fmt.Fprintf(&userData, "%s", indentMultiline(cloudInitReadyScript(workRoot), "          "))
	fmt.Fprintf(&userData, "    runcmd:\n")
	fmt.Fprintf(&userData, "      - [mkdir, -p, %s, /var/cache/crabbox/pnpm, /var/cache/crabbox/npm, /var/lib/crabbox]\n", yamlSingleQuotedScalar(workRoot))
	fmt.Fprintf(&userData, "      - [chown, -R, %s, %s, /var/cache/crabbox]\n", yamlSingleQuotedScalar(user+":"+user), yamlSingleQuotedScalar(workRoot))
	fmt.Fprintf(&userData, "      - |\n")
	fmt.Fprintf(&userData, "        systemctl enable ssh || true\n")
	fmt.Fprintf(&userData, "        systemctl disable --now ssh.socket || true\n")
	fmt.Fprintf(&userData, "        timeout 30s systemctl restart ssh.service || timeout 30s systemctl restart ssh || true\n")
	fmt.Fprintf(&userData, "      - |\n")
	fmt.Fprintf(&userData, "        systemctl enable xe-daemon || true\n")
	fmt.Fprintf(&userData, "        timeout 30s systemctl restart xe-daemon || true\n")
	fmt.Fprintf(&userData, "      - [touch, /var/lib/crabbox/bootstrapped]\n")
	fmt.Fprintf(&userData, "      - [/usr/local/bin/crabbox-ready]\n")
	fmt.Fprintf(&userData, "  late-commands:\n")
	fmt.Fprintf(&userData, "    - curtin in-target -- systemctl enable xe-daemon || true\n")
	fmt.Fprintf(&userData, "  shutdown: reboot\n")
	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: crabbox-%s\n", shellSafeCloudInitScalar(leaseID), shellSafeCloudInitScalar(slug))
	return xcpNgLinuxAutoinstallPayload{UserData: userData.String(), MetaData: metaData}, nil
}

func newWindowsAutounattendPayload(cfg Config, leaseID, slug, publicKey, initialPassword string) (xcpNgWindowsAutounattendPayload, error) {
	rawUser := strings.TrimSpace(core.Blank(cfg.XCPNg.User, cfg.SSHUser))
	if rawUser == "" {
		return xcpNgWindowsAutounattendPayload{}, exit(2, "xcp-ng windows autounattend user is required")
	}
	user := windowsAccountName(rawUser)
	if user == "" {
		return xcpNgWindowsAutounattendPayload{}, exit(2, "xcp-ng windows autounattend user is required")
	}
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return xcpNgWindowsAutounattendPayload{}, exit(2, "xcp-ng windows autounattend public key is required")
	}
	initialPassword = strings.TrimSpace(initialPassword)
	if initialPassword == "" {
		return xcpNgWindowsAutounattendPayload{}, exit(2, "xcp-ng windows autounattend password is required")
	}
	cfgCopy := cfg
	cfgCopy.TargetOS = "windows"
	cfgCopy.WindowsMode = "normal"
	cfgCopy.SSHUser = user
	cfgCopy.XCPNg.User = user
	workRoot := strings.TrimSpace(core.Blank(cfgCopy.XCPNg.WorkRoot, cfgCopy.WorkRoot))
	if strings.HasPrefix(workRoot, "/") {
		cfgCopy.WorkRoot = ""
	} else if workRoot != "" {
		cfgCopy.WorkRoot = workRoot
	}
	bootstrap := core.WindowsBootstrapPowerShell(cfgCopy, publicKey)
	bootstrapCommand := core.PowershellCommand(`$volume = Get-Volume | Where-Object { $_.FileSystemLabel -eq 'CRABBOXWIN' } | Select-Object -First 1
if (-not $volume) { throw "Crabbox Windows answer media volume not found" }
$scriptRoot = $volume.DriveLetter + ":\"
$scriptPath = Join-Path $scriptRoot "CRABBOX-BOOTSTRAP.PS1"
if (-not (Test-Path -LiteralPath $scriptPath)) { throw "Crabbox bootstrap script not found on answer media" }
& $scriptPath`)
	computerName := windowsComputerName(slug)

	var answer bytes.Buffer
	fmt.Fprintf(&answer, "<?xml version=\"1.0\" encoding=\"utf-8\"?>\n")
	fmt.Fprintf(&answer, "<unattend xmlns=\"urn:schemas-microsoft-com:unattend\" xmlns:wcm=\"http://schemas.microsoft.com/WMIConfig/2002/State\">\n")
	fmt.Fprintf(&answer, "  <settings pass=\"windowsPE\">\n")
	fmt.Fprintf(&answer, "    <component name=\"Microsoft-Windows-International-Core-WinPE\" processorArchitecture=\"amd64\" publicKeyToken=\"31bf3856ad364e35\" language=\"neutral\" versionScope=\"nonSxS\">\n")
	fmt.Fprintf(&answer, "      <SetupUILanguage><UILanguage>en-US</UILanguage></SetupUILanguage>\n")
	fmt.Fprintf(&answer, "      <InputLocale>en-US</InputLocale>\n")
	fmt.Fprintf(&answer, "      <SystemLocale>en-US</SystemLocale>\n")
	fmt.Fprintf(&answer, "      <UILanguage>en-US</UILanguage>\n")
	fmt.Fprintf(&answer, "      <UserLocale>en-US</UserLocale>\n")
	fmt.Fprintf(&answer, "    </component>\n")
	fmt.Fprintf(&answer, "    <component name=\"Microsoft-Windows-Setup\" processorArchitecture=\"amd64\" publicKeyToken=\"31bf3856ad364e35\" language=\"neutral\" versionScope=\"nonSxS\">\n")
	fmt.Fprintf(&answer, "      <Diagnostics><OptIn>false</OptIn></Diagnostics>\n")
	fmt.Fprintf(&answer, "      <DiskConfiguration>\n")
	fmt.Fprintf(&answer, "        <Disk wcm:action=\"add\">\n")
	fmt.Fprintf(&answer, "          <DiskID>0</DiskID>\n")
	fmt.Fprintf(&answer, "          <WillWipeDisk>true</WillWipeDisk>\n")
	fmt.Fprintf(&answer, "          <CreatePartitions>\n")
	fmt.Fprintf(&answer, "            <CreatePartition wcm:action=\"add\"><Order>1</Order><Type>EFI</Type><Size>100</Size></CreatePartition>\n")
	fmt.Fprintf(&answer, "            <CreatePartition wcm:action=\"add\"><Order>2</Order><Type>MSR</Type><Size>16</Size></CreatePartition>\n")
	fmt.Fprintf(&answer, "            <CreatePartition wcm:action=\"add\"><Order>3</Order><Type>Primary</Type><Extend>true</Extend></CreatePartition>\n")
	fmt.Fprintf(&answer, "          </CreatePartitions>\n")
	fmt.Fprintf(&answer, "          <ModifyPartitions>\n")
	fmt.Fprintf(&answer, "            <ModifyPartition wcm:action=\"add\"><Order>1</Order><PartitionID>1</PartitionID><Label>System</Label><Format>FAT32</Format></ModifyPartition>\n")
	fmt.Fprintf(&answer, "            <ModifyPartition wcm:action=\"add\"><Order>2</Order><PartitionID>3</PartitionID><Label>Windows</Label><Letter>C</Letter><Format>NTFS</Format></ModifyPartition>\n")
	fmt.Fprintf(&answer, "          </ModifyPartitions>\n")
	fmt.Fprintf(&answer, "        </Disk>\n")
	fmt.Fprintf(&answer, "        <WillShowUI>OnError</WillShowUI>\n")
	fmt.Fprintf(&answer, "      </DiskConfiguration>\n")
	fmt.Fprintf(&answer, "      <ImageInstall>\n")
	fmt.Fprintf(&answer, "        <OSImage>\n")
	fmt.Fprintf(&answer, "          <InstallFrom>\n")
	fmt.Fprintf(&answer, "            <MetaData wcm:action=\"add\"><Key>/IMAGE/INDEX</Key><Value>1</Value></MetaData>\n")
	fmt.Fprintf(&answer, "          </InstallFrom>\n")
	fmt.Fprintf(&answer, "          <InstallTo><DiskID>0</DiskID><PartitionID>3</PartitionID></InstallTo>\n")
	fmt.Fprintf(&answer, "          <WillShowUI>OnError</WillShowUI>\n")
	fmt.Fprintf(&answer, "        </OSImage>\n")
	fmt.Fprintf(&answer, "      </ImageInstall>\n")
	fmt.Fprintf(&answer, "      <UserData>\n")
	fmt.Fprintf(&answer, "        <AcceptEula>true</AcceptEula>\n")
	fmt.Fprintf(&answer, "        <FullName>Crabbox</FullName>\n")
	fmt.Fprintf(&answer, "        <Organization>Crabbox</Organization>\n")
	fmt.Fprintf(&answer, "      </UserData>\n")
	fmt.Fprintf(&answer, "    </component>\n")
	fmt.Fprintf(&answer, "  </settings>\n")
	fmt.Fprintf(&answer, "  <settings pass=\"specialize\">\n")
	fmt.Fprintf(&answer, "    <component name=\"Microsoft-Windows-Shell-Setup\" processorArchitecture=\"amd64\" publicKeyToken=\"31bf3856ad364e35\" language=\"neutral\" versionScope=\"nonSxS\">\n")
	fmt.Fprintf(&answer, "      <ComputerName>%s</ComputerName>\n", xmlEscapeScalar(computerName))
	fmt.Fprintf(&answer, "      <TimeZone>UTC</TimeZone>\n")
	fmt.Fprintf(&answer, "      <RegisteredOwner>Crabbox</RegisteredOwner>\n")
	fmt.Fprintf(&answer, "      <RegisteredOrganization>Crabbox</RegisteredOrganization>\n")
	fmt.Fprintf(&answer, "    </component>\n")
	fmt.Fprintf(&answer, "  </settings>\n")
	fmt.Fprintf(&answer, "  <settings pass=\"oobeSystem\">\n")
	fmt.Fprintf(&answer, "    <component name=\"Microsoft-Windows-International-Core\" processorArchitecture=\"amd64\" publicKeyToken=\"31bf3856ad364e35\" language=\"neutral\" versionScope=\"nonSxS\">\n")
	fmt.Fprintf(&answer, "      <InputLocale>en-US</InputLocale>\n")
	fmt.Fprintf(&answer, "      <SystemLocale>en-US</SystemLocale>\n")
	fmt.Fprintf(&answer, "      <UILanguage>en-US</UILanguage>\n")
	fmt.Fprintf(&answer, "      <UserLocale>en-US</UserLocale>\n")
	fmt.Fprintf(&answer, "    </component>\n")
	fmt.Fprintf(&answer, "    <component name=\"Microsoft-Windows-Shell-Setup\" processorArchitecture=\"amd64\" publicKeyToken=\"31bf3856ad364e35\" language=\"neutral\" versionScope=\"nonSxS\">\n")
	fmt.Fprintf(&answer, "      <OOBE>\n")
	fmt.Fprintf(&answer, "        <HideEULAPage>true</HideEULAPage>\n")
	fmt.Fprintf(&answer, "        <HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>\n")
	fmt.Fprintf(&answer, "        <NetworkLocation>Work</NetworkLocation>\n")
	fmt.Fprintf(&answer, "        <ProtectYourPC>3</ProtectYourPC>\n")
	fmt.Fprintf(&answer, "      </OOBE>\n")
	fmt.Fprintf(&answer, "      <AutoLogon>\n")
	fmt.Fprintf(&answer, "        <Enabled>true</Enabled>\n")
	fmt.Fprintf(&answer, "        <LogonCount>1</LogonCount>\n")
	fmt.Fprintf(&answer, "        <Username>%s</Username>\n", xmlEscapeScalar(user))
	fmt.Fprintf(&answer, "        <Password><Value>%s</Value><PlainText>true</PlainText></Password>\n", xmlEscapeScalar(initialPassword))
	fmt.Fprintf(&answer, "      </AutoLogon>\n")
	fmt.Fprintf(&answer, "      <UserAccounts>\n")
	fmt.Fprintf(&answer, "        <LocalAccounts>\n")
	fmt.Fprintf(&answer, "          <LocalAccount wcm:action=\"add\">\n")
	fmt.Fprintf(&answer, "            <Name>%s</Name>\n", xmlEscapeScalar(user))
	fmt.Fprintf(&answer, "            <DisplayName>%s</DisplayName>\n", xmlEscapeScalar(user))
	fmt.Fprintf(&answer, "            <Description>Crabbox Windows ISO E2E</Description>\n")
	fmt.Fprintf(&answer, "            <Group>Administrators</Group>\n")
	fmt.Fprintf(&answer, "            <Password><Value>%s</Value><PlainText>true</PlainText></Password>\n", xmlEscapeScalar(initialPassword))
	fmt.Fprintf(&answer, "          </LocalAccount>\n")
	fmt.Fprintf(&answer, "        </LocalAccounts>\n")
	fmt.Fprintf(&answer, "      </UserAccounts>\n")
	fmt.Fprintf(&answer, "      <FirstLogonCommands>\n")
	fmt.Fprintf(&answer, "        <SynchronousCommand wcm:action=\"add\">\n")
	fmt.Fprintf(&answer, "          <Order>1</Order>\n")
	fmt.Fprintf(&answer, "          <Description>Run Crabbox bootstrap</Description>\n")
	fmt.Fprintf(&answer, "          <RequiresUserInput>false</RequiresUserInput>\n")
	fmt.Fprintf(&answer, "          <CommandLine>%s</CommandLine>\n", xmlEscapeScalar(bootstrapCommand))
	fmt.Fprintf(&answer, "        </SynchronousCommand>\n")
	fmt.Fprintf(&answer, "      </FirstLogonCommands>\n")
	fmt.Fprintf(&answer, "    </component>\n")
	fmt.Fprintf(&answer, "  </settings>\n")
	fmt.Fprintf(&answer, "</unattend>\n")

	return xcpNgWindowsAutounattendPayload{
		AnswerXML:           answer.String(),
		BootstrapPowerShell: bootstrap,
		Username:            user,
	}, nil
}

func cloudInitSSHPortConfig(cfg Config) string {
	portLines := ""
	for _, port := range xcpNgSSHPortCandidates(cfg.SSHPort, cfg.SSHFallbackPorts) {
		portLines += fmt.Sprintf("      Port %s\n", port)
	}
	return portLines
}

func cloudInitReadyScript(workRoot string) string {
	return fmt.Sprintf("#!/usr/bin/env bash\nset -euo pipefail\ngit --version >/dev/null\nrsync --version >/dev/null\ncurl --version >/dev/null\njq --version >/dev/null\ntest -f /var/lib/crabbox/bootstrapped\ntest -w %s\n", shellQuote(workRoot))
}

func indentMultiline(text, indent string) string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return indent + "\n"
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n") + "\n"
}

func xcpNgSSHPortCandidates(port string, fallbackPorts []string) []string {
	if fallbackPorts == nil {
		fallbackPorts = []string{"22"}
	}
	seen := make(map[string]bool, len(fallbackPorts)+1)
	out := make([]string, 0, len(fallbackPorts)+1)
	for _, candidate := range append([]string{port}, fallbackPorts...) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	return out
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func shellSafeCloudInitScalar(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

func yamlSingleQuotedScalar(value string) string {
	value = shellSafeCloudInitScalar(value)
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func xmlEscapeScalar(value string) string {
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(shellSafeCloudInitScalar(value))); err != nil {
		return shellSafeCloudInitScalar(value)
	}
	return escaped.String()
}

func windowsAccountName(value string) string {
	value = shellSafeCloudInitScalar(value)
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			builder.WriteRune(r)
		}
	}
	name := strings.Trim(builder.String(), "-_")
	if name == "" {
		return ""
	}
	if len(name) > 20 {
		return name[:20]
	}
	return name
}

func windowsComputerName(slug string) string {
	slug = strings.ToUpper(shellSafeCloudInitScalar(slug))
	var builder strings.Builder
	builder.WriteString("CRABBOX-")
	for _, r := range slug {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			builder.WriteRune(r)
		}
	}
	name := strings.Trim(builder.String(), "-")
	if name == "" {
		name = "CRABBOX-WIN"
	}
	if len(name) > 15 {
		name = strings.Trim(name[:15], "-")
	}
	if name == "" {
		return "CRABBOXWIN"
	}
	return name
}

func generateWindowsAutoLogonPassword() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "Cbx1!" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func configDriveLabels(base map[string]string) map[string]string {
	labels := make(map[string]string, len(base)+2)
	for key, value := range base {
		labels[key] = value
	}
	labels["resource"] = "config-drive"
	labels["cleanup_with_vm"] = "true"
	return labels
}

func vmDiskLabels(base map[string]string) map[string]string {
	labels := make(map[string]string, len(base)+2)
	for key, value := range base {
		labels[key] = value
	}
	labels["resource"] = "vm-disk"
	labels["cleanup_with_vm"] = "true"
	return labels
}

func isoMediaLabels(base map[string]string) map[string]string {
	labels := make(map[string]string, len(base)+2)
	for key, value := range base {
		labels[key] = value
	}
	labels["resource"] = "installer-media"
	labels["cleanup_with_vm"] = "true"
	return labels
}

func buildConfigDriveImage(payload xcpNgCloudInitPayload) ([]byte, error) {
	files := []fatFile{
		{Name: "user-data", Data: []byte(payload.UserData)},
		{Name: "meta-data", Data: []byte(payload.MetaData)},
	}
	return buildFAT16Image("cidata", files)
}

type fatFile struct {
	Name string
	Data []byte
}

func buildFAT16Image(label string, files []fatFile) ([]byte, error) {
	const (
		bytesPerSector    = 512
		sectorsPerCluster = 4
		reservedSectors   = 1
		fatCount          = 2
		rootEntries       = 512
		totalSectors      = 20480
		sectorsPerFAT     = 40
	)
	rootDirSectors := rootEntries * 32 / bytesPerSector
	firstDataSector := reservedSectors + fatCount*sectorsPerFAT + rootDirSectors
	clusterSize := sectorsPerCluster * bytesPerSector
	dataClusters := (totalSectors - firstDataSector) / sectorsPerCluster
	fatEntries := sectorsPerFAT * bytesPerSector / 2
	image := make([]byte, totalSectors*bytesPerSector)
	boot := image[:bytesPerSector]
	boot[0] = 0xeb
	boot[1] = 0x3c
	boot[2] = 0x90
	copy(boot[3:11], []byte("CRABBOX "))
	binary.LittleEndian.PutUint16(boot[11:13], bytesPerSector)
	boot[13] = sectorsPerCluster
	binary.LittleEndian.PutUint16(boot[14:16], reservedSectors)
	boot[16] = fatCount
	binary.LittleEndian.PutUint16(boot[17:19], rootEntries)
	binary.LittleEndian.PutUint16(boot[19:21], totalSectors)
	boot[21] = 0xf8
	binary.LittleEndian.PutUint16(boot[22:24], sectorsPerFAT)
	binary.LittleEndian.PutUint16(boot[24:26], 63)
	binary.LittleEndian.PutUint16(boot[26:28], 255)
	boot[36] = 0x80
	boot[38] = 0x29
	binary.LittleEndian.PutUint32(boot[39:43], 0x43525842)
	copyPadded(boot[43:54], strings.ToUpper(label), ' ')
	copy(boot[54:62], []byte("FAT16   "))
	boot[510] = 0x55
	boot[511] = 0xaa

	fatStart := reservedSectors * bytesPerSector
	rootStart := (reservedSectors + fatCount*sectorsPerFAT) * bytesPerSector
	dataStart := firstDataSector * bytesPerSector
	fat := image[fatStart : fatStart+sectorsPerFAT*bytesPerSector]
	binary.LittleEndian.PutUint16(fat[0:2], 0xfff8)
	binary.LittleEndian.PutUint16(fat[2:4], 0xffff)
	nextCluster := 2
	rootOffset := 0
	root := image[rootStart : rootStart+rootDirSectors*bytesPerSector]
	labelEntry := root[rootOffset : rootOffset+32]
	copyPadded(labelEntry[0:11], strings.ToUpper(label), ' ')
	labelEntry[11] = 0x08
	rootOffset += 32
	for i, file := range files {
		if strings.TrimSpace(file.Name) == "" {
			return nil, exit(2, "config-drive file name is required")
		}
		cluster := nextCluster
		clusterCount := (len(file.Data) + clusterSize - 1) / clusterSize
		if clusterCount == 0 {
			clusterCount = 1
		}
		if cluster-2+clusterCount > dataClusters || cluster+clusterCount > fatEntries {
			return nil, exit(2, "config-drive payload is too large")
		}
		short := fmt.Sprintf("CRAB%04dTXT", i+1)
		checksum := fatShortChecksum([]byte(short))
		lfnEntries := fatLongNameEntries(file.Name, checksum)
		if rootOffset+(len(lfnEntries)+1)*32 > len(root) {
			return nil, exit(2, "config-drive directory is too large")
		}
		for c := 0; c < clusterCount; c++ {
			entry := (cluster + c) * 2
			next := uint16(0xffff)
			if c+1 < clusterCount {
				next = uint16(cluster + c + 1)
			}
			binary.LittleEndian.PutUint16(fat[entry:entry+2], next)
		}
		dataOffset := dataStart + (cluster-2)*clusterSize
		copy(image[dataOffset:dataOffset+len(file.Data)], file.Data)
		for _, entry := range lfnEntries {
			copy(root[rootOffset:rootOffset+32], entry[:])
			rootOffset += 32
		}
		entry := root[rootOffset : rootOffset+32]
		copy(entry[0:11], []byte(short))
		entry[11] = 0x20
		binary.LittleEndian.PutUint16(entry[26:28], uint16(cluster))
		binary.LittleEndian.PutUint32(entry[28:32], uint32(len(file.Data)))
		rootOffset += 32
		nextCluster += clusterCount
	}
	copy(image[fatStart+sectorsPerFAT*bytesPerSector:fatStart+2*sectorsPerFAT*bytesPerSector], fat)
	return image, nil
}

func copyPadded(dst []byte, value string, pad byte) {
	for i := range dst {
		dst[i] = pad
	}
	copy(dst, []byte(value))
}

func fatShortChecksum(short []byte) byte {
	var sum byte
	for _, b := range short {
		sum = ((sum & 1) << 7) + (sum >> 1) + b
	}
	return sum
}

func fatLongNameEntries(name string, checksum byte) [][32]byte {
	runes := []rune(name)
	chunks := (len(runes) + 12) / 13
	entries := make([][32]byte, 0, chunks)
	for i := chunks - 1; i >= 0; i-- {
		var entry [32]byte
		sequence := byte(i + 1)
		if i == chunks-1 {
			sequence |= 0x40
		}
		entry[0] = sequence
		entry[11] = 0x0f
		entry[13] = checksum
		start := i * 13
		end := start + 13
		if end > len(runes) {
			end = len(runes)
		}
		writeLongNameRunes(entry[:], runes[start:end])
		entries = append(entries, entry)
	}
	return entries
}

func writeLongNameRunes(entry []byte, runes []rune) {
	positions := []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}
	for i, pos := range positions {
		value := uint16(0xffff)
		if i < len(runes) {
			value = uint16(runes[i])
		} else if i == len(runes) {
			value = 0x0000
		}
		binary.LittleEndian.PutUint16(entry[pos:pos+2], value)
	}
}
