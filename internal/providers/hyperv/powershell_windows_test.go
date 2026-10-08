//go:build windows

package hyperv

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPowerShellCredentialPreludeDoesNotRequireSecurityModule(t *testing.T) {
	plainText := strings.Repeat("x", 27)
	script := powershellCredentialPrelude("crabbox") +
		`Write-Output $cred.UserName; Write-Output $cred.Password.Length`

	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	command.Env = append(os.Environ(), "PSModulePath=", "_CRABBOX_GP="+plainText)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell credential construction failed without PSModulePath: %v\n%s", err, output)
	}
	lines := strings.Fields(string(output))
	if len(lines) != 2 || lines[0] != "crabbox" || lines[1] != "27" {
		t.Fatalf("credential output = %q, want user and password length", output)
	}
}

func TestHyperVCacheCreateScriptReportsDiskIdentity(t *testing.T) {
	// The script mounts the new VHDX, and Mount-VHD requires an elevated
	// process. Hyper-V Administrators membership is enough to inspect the host
	// but not to attach a disk, so probe for elevation too or this skips
	// nothing and then fails on a privilege error.
	probe := exec.Command(
		"powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		`$ErrorActionPreference='Stop'
Get-VMHost | Out-Null
Get-Command New-VHD -ErrorAction Stop | Out-Null
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw 'Mount-VHD requires an elevated process'
}`,
	)
	if output, err := probe.CombinedOutput(); err != nil {
		t.Skipf("elevated Hyper-V host is unavailable: %v\n%s", err, output)
	}

	vhdPath := filepath.Join(t.TempDir(), "cache.vhdx")
	command := exec.Command(
		"powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		hypervCacheCreateScript(vhdPath, 64*1024*1024),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("create cache VHDX: %v\n%s", err, output)
	}
	var created hypervCacheCreateOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(output))), &created); err != nil {
		t.Fatalf("parse cache VHDX identity: %v\n%s", err, output)
	}
	if strings.TrimSpace(created.DiskID) == "" {
		t.Fatalf("cache VHDX identity is empty: %s", output)
	}
	if _, err := os.Stat(vhdPath); err != nil {
		t.Fatalf("cache VHDX was not created: %v", err)
	}
}
