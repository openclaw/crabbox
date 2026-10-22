//go:build windows

package hyperv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func powershellCommand(path, script string) *exec.Cmd {
	cmd := exec.Command(path, "-NoProfile", "-NonInteractive", "-Command", script)
	// Test isolation can rewrite PSModulePath; Set-Acl needs the stock module directory.
	cmd.Env = append(os.Environ(), "PSModulePath="+filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "Modules"))
	return cmd
}

func TestGrantExportRootSystemScriptKeepsPrivateDACLAndAddsSystem(t *testing.T) {
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe unavailable")
	}
	dir := t.TempDir()
	setup := `$u=[Security.Principal.WindowsIdentity]::GetCurrent().User; $a=[Security.AccessControl.DirectorySecurity]::new(); ` +
		`$a.SetAccessRuleProtection($true,$false); ` +
		`$a.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new($u,'FullControl','ContainerInherit,ObjectInherit','None','Allow')); ` +
		`[IO.DirectoryInfo]::new('` + escapePSString(dir) + `').SetAccessControl($a)`
	if out, err := powershellCommand(powershell, setup).CombinedOutput(); err != nil {
		t.Fatalf("setup private DACL: %v: %s", err, out)
	}
	if out, err := powershellCommand(powershell, "$ErrorActionPreference='Stop'; "+grantExportRootSystemScript(dir)).CombinedOutput(); err != nil {
		t.Fatalf("grant script: %v: %s", err, out)
	}
	inspect := `$a=[IO.DirectoryInfo]::new('` + escapePSString(dir) + `').GetAccessControl([Security.AccessControl.AccessControlSections]::Access); ` +
		`"protected=$($a.AreAccessRulesProtected)"; ` +
		`$a.Access | ForEach-Object { "$($_.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value)=$($_.FileSystemRights)" }`
	out, err := powershellCommand(powershell, inspect).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect DACL: %v: %s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "protected=True") {
		t.Fatalf("inheritance must stay protected: %s", text)
	}
	if !strings.Contains(text, "S-1-5-18=FullControl") {
		t.Fatalf("SYSTEM was not granted: %s", text)
	}
	if strings.Count(text, "=FullControl") != 2 {
		t.Fatalf("DACL must contain exactly the owner and SYSTEM: %s", text)
	}
}
