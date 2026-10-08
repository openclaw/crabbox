package external

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func assertPrivateDir(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", path)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s permissions=%#o want=0700", path, got)
		}
		return
	}
	if err := validatePrivatePathWindows(path); err != nil {
		t.Fatal(err)
	}
}

func validatePrivatePathWindows(path string) error {
	const script = `& {
		$path = $env:CRABBOX_TEST_PRIVATE_PATH
		$isDirectory = [System.IO.Directory]::Exists($path)
		$acl = if ($isDirectory) { [System.IO.Directory]::GetAccessControl($path) } else { [System.IO.File]::GetAccessControl($path) }
		$currentSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
		$ownerSid = $acl.GetOwner([System.Security.Principal.SecurityIdentifier]).Value
		if ($ownerSid -ne $currentSid) {
			throw "owner is not the current user"
		}
		if (-not $acl.AreAccessRulesProtected) {
			throw "access-control list is not protected"
		}
		$allowed = @($currentSid, 'S-1-5-18', 'S-1-5-32-544')
		$hasOwner = $false
		foreach ($rule in $acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier])) {
			if ($rule.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) {
				continue
			}
			$sid = $rule.IdentityReference.Value
			if ($sid -eq 'S-1-3-0' -and (($rule.PropagationFlags -band [System.Security.AccessControl.PropagationFlags]::InheritOnly) -ne 0)) {
				continue
			}
			if ($sid -eq $currentSid) {
				$hasOwner = $true
				continue
			}
			if ($allowed -contains $sid) {
				continue
			}
			throw "unexpected allow ACE for $sid"
		}
		if (-not $hasOwner) {
			throw "owner allow ACE is missing"
		}
	}`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "CRABBOX_TEST_PRIVATE_PATH="+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("validate Windows ACL for %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
