package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWindowsInteractiveTaskDirectoryIsProtected(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows ACL behavior")
	}
	root := filepath.Join(t.TempDir(), "capture")
	scriptPath := filepath.Join(t.TempDir(), "protect.ps1")
	script := windowsInteractiveScheduledTaskPowerShell() + `
$path = ` + psQuote(root) + `
New-Item -ItemType Directory -Path $path | Out-Null
$initialACL = [IO.DirectoryInfo]::new($path).GetAccessControl()
$initialACL.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new(
  [Security.Principal.SecurityIdentifier]::new("S-1-5-32-545"),
  [Security.AccessControl.FileSystemRights]::ReadAndExecute,
  [Security.AccessControl.InheritanceFlags]"ContainerInherit, ObjectInherit",
  [Security.AccessControl.PropagationFlags]::None, [Security.AccessControl.AccessControlType]::Allow))
[IO.DirectoryInfo]::new($path).SetAccessControl($initialACL)
Protect-CrabboxInteractiveDirectory $path
$acl = [IO.DirectoryInfo]::new($path).GetAccessControl()
$rules = @($acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]) | ForEach-Object {
  [pscustomobject]@{
    Sid = $_.IdentityReference.Value
    Rights = $_.FileSystemRights.ToString()
    Type = $_.AccessControlType.ToString()
    Inherited = $_.IsInherited
  }
})
[pscustomobject]@{
  Protected = $acl.AreAccessRulesProtected
  UserSid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
  Rules = $rules
} | ConvertTo-Json -Depth 4 -Compress
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath).CombinedOutput()
	if err != nil {
		t.Fatalf("protect interactive task directory: %v: %s", err, strings.TrimSpace(string(output)))
	}
	var acl struct {
		Protected bool   `json:"Protected"`
		UserSID   string `json:"UserSid"`
		Rules     []struct {
			SID       string `json:"Sid"`
			Rights    string `json:"Rights"`
			Type      string `json:"Type"`
			Inherited bool   `json:"Inherited"`
		} `json:"Rules"`
	}
	if err := json.Unmarshal(output, &acl); err != nil {
		t.Fatalf("parse capture ACL %q: %v", output, err)
	}
	expected := map[string]bool{acl.UserSID: true, "S-1-5-32-544": true, "S-1-5-18": true}
	if !acl.Protected || len(acl.Rules) != len(expected) {
		t.Fatalf("capture ACL protected=%v rules=%#v", acl.Protected, acl.Rules)
	}
	for _, rule := range acl.Rules {
		if !expected[rule.SID] || rule.Rights != "FullControl" || rule.Type != "Allow" || rule.Inherited {
			t.Fatalf("unexpected capture ACL rule: %#v", rule)
		}
		delete(expected, rule.SID)
	}
	if len(expected) != 0 {
		t.Fatalf("capture ACL missing SIDs: %#v", expected)
	}
}

func TestWindowsCredentialReadersPreserveExactUTF8(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell credential reader behavior")
	}
	credentials := map[string][]byte{
		"unicode-and-surrounding-whitespace": []byte(" \u03c0-desktop-proof \t"),
		"whitespace-only":                    []byte(" \t "),
	}
	readers := map[string]func(string) string{
		"runner":     windowsRunnerCredentialReadPowerShell,
		"screenshot": windowsScreenshotCredentialReadPowerShell,
		"video":      windowsVideoCredentialReadPowerShell,
	}
	for credentialName, credential := range credentials {
		for readerName, reader := range readers {
			t.Run(credentialName+"/"+readerName, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "windows.password")
				if err := os.WriteFile(path, credential, 0o600); err != nil {
					t.Fatal(err)
				}
				scriptPath := filepath.Join(root, "read.ps1")
				script := reader(path) + `
[Console]::Write([Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($logonValue)))
`
				if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
					t.Fatal(err)
				}
				output, err := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath).CombinedOutput()
				if err != nil {
					t.Fatalf("read exact credential: %v: %s", err, strings.TrimSpace(string(output)))
				}
				decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(output)))
				if err != nil {
					t.Fatalf("decode reader output %q: %v", output, err)
				}
				if !bytes.Equal(decoded, credential) {
					t.Fatalf("credential bytes=%x want=%x", decoded, credential)
				}
			})
		}
	}
}

func TestWindowsCredentialReadersRejectZeroLength(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell credential reader behavior")
	}
	readers := map[string]func(string) string{
		"runner":     windowsRunnerCredentialReadPowerShell,
		"screenshot": windowsScreenshotCredentialReadPowerShell,
		"video":      windowsVideoCredentialReadPowerShell,
	}
	for name, reader := range readers {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "windows.password")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			scriptPath := filepath.Join(root, "read.ps1")
			if err := os.WriteFile(scriptPath, []byte(reader(path)), 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath).CombinedOutput()
			if err == nil {
				t.Fatal("zero-length credential was accepted")
			}
			if !strings.Contains(string(output), "credential file is empty") {
				t.Fatalf("zero-length error=%s", strings.TrimSpace(string(output)))
			}
		})
	}
}
