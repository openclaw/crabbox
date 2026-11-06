package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The runner installer runs as the lease's administrative account. Staging it
// where another guest account can create, replace or rename files would let that
// account run code as the administrator, so it must land in a fresh directory in
// the account's own profile with a private, non-inherited ACL.
func TestWindowsRunnerScriptStagingIsPrivateAndFresh(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell is required")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell is required")
	}
	profile := t.TempDir()
	// Run each generated guest command locally with the profile redirected.
	runLocal := func(ctx context.Context, _ SSHTarget, remote string, input io.Reader, stdout, stderr io.Writer) error {
		fields := strings.Fields(remote)
		cmd := exec.CommandContext(ctx, fields[0], fields[1:]...)
		cmd.Env = windowsPowerShellTestEnv(t, "USERPROFILE="+profile)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = input, stdout, stderr
		return cmd.Run()
	}
	copied := ""
	copyLocal := func(_ context.Context, _ SSHTarget, local, remote string) error {
		copied = remote
		data, err := os.ReadFile(local)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.FromSlash(remote), data, 0o600)
	}
	staging := windowsRunnerScriptStaging{run: runLocal, copy: copyLocal}
	const script = "Write-Output 'installer'\n"

	remotePath, cleanup, err := staging.upload(context.Background(), SSHTarget{TargetOS: targetWindows}, script)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(remotePath)
	if filepath.Dir(dir) != profile || !strings.HasPrefix(filepath.Base(dir), windowsRunnerScriptDirPrefix) || filepath.Base(remotePath) != "install-runner.ps1" {
		t.Fatalf("installer staged at %s, want a fresh directory in the profile %s", remotePath, profile)
	}
	if filepath.FromSlash(copied) != remotePath {
		t.Fatalf("copied to %q, staged path %q", copied, remotePath)
	}
	data, err := os.ReadFile(remotePath)
	if err != nil || !bytes.Equal(data, append([]byte{0xEF, 0xBB, 0xBF}, script...)) {
		t.Fatalf("staged installer=%q err=%v", data, err)
	}

	aclScript := `$acl = Get-Acl -LiteralPath $env:STAGING_DIR
$rules = @($acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
[pscustomobject]@{ protected = $acl.AreAccessRulesProtected; inherited = @($rules | Where-Object IsInherited).Count; sids = @($rules | ForEach-Object { $_.IdentityReference.Value } | Sort-Object -Unique); user = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value } | ConvertTo-Json -Compress`
	cmd := exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", aclScript)
	cmd.Env = windowsPowerShellModuleTestEnv(t, "STAGING_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read staging ACL: %v\n%s", err, out)
	}
	var acl struct {
		Protected bool     `json:"protected"`
		Inherited int      `json:"inherited"`
		SIDs      []string `json:"sids"`
		User      string   `json:"user"`
	}
	if err := json.Unmarshal(out, &acl); err != nil {
		t.Fatalf("parse staging ACL %q: %v", out, err)
	}
	want := map[string]bool{acl.User: true, "S-1-5-32-544": true, "S-1-5-18": true}
	if !acl.Protected || acl.Inherited != 0 || len(acl.SIDs) != len(want) {
		t.Fatalf("staging ACL is not private: %+v", acl)
	}
	for _, sid := range acl.SIDs {
		if !want[sid] {
			t.Fatalf("staging ACL grants %s: %+v", sid, acl)
		}
	}

	// A directory that already exists, for example one another account created
	// first, is refused rather than reused.
	var stderr bytes.Buffer
	if err := runLocal(context.Background(), SSHTarget{}, PowershellCommand(windowsRunnerScriptStagingPowerShell(filepath.Base(dir))), nil, io.Discard, &stderr); err == nil {
		t.Fatal("staging reused an existing directory")
	}

	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("staging directory survived cleanup: %v", err)
	}
}

// windowsPowerShellTestEnv is the environment for a Windows PowerShell child
// of these tests. The test binary isolates user directories, so PowerShell would
// otherwise write its module-analysis cache relative to the package directory.
func windowsPowerShellTestEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	env := append(os.Environ(), "PSModuleAnalysisCachePath="+filepath.Join(t.TempDir(), "ModuleAnalysisCache"))
	return append(env, extra...)
}

// windowsPowerShellModuleTestEnv also drops a PowerShell 7 PSModulePath
// inherited from the host, which breaks Windows PowerShell's own Get-Acl and
// job cmdlets.
func windowsPowerShellModuleTestEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	return withoutEnv(windowsPowerShellTestEnv(t, extra...), "PSModulePath")
}

func withoutEnv(env []string, name string) []string {
	kept := env[:0:0]
	for _, entry := range env {
		if key, _, _ := strings.Cut(entry, "="); !strings.EqualFold(key, name) {
			kept = append(kept, entry)
		}
	}
	return kept
}
