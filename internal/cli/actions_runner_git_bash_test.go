package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Workflow steps with `shell: bash` fail on a native Windows runner unless the
// runner can resolve Git Bash. The runner setup installs the pinned Git for
// Windows only when bash.exe is missing, verifies it, and puts Git\bin first on
// the runner's PATH.
func TestWindowsRunnerGitBashProvisioning(t *testing.T) {
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell not available")
	}
	// The download, hash, and installer are stubbed; Test-Path, Join-Path and
	// Remove-Item run for real against a temporary ProgramFiles.
	stubs := `$ErrorActionPreference = "Stop"
$env:ProgramFiles = $env:FAKE_PROGRAM_FILES
$script:events = [System.Collections.Generic.List[string]]::new()
function Write-CrabboxRunnerStage([string]$Stage) { $script:events.Add("stage " + $Stage) }
function Invoke-WebRequest { param([string]$Uri, [string]$OutFile, [switch]$UseBasicParsing, [int]$TimeoutSec) $script:events.Add("download " + $Uri); Set-Content -LiteralPath $OutFile -Value "installer" }
function Get-FileHash { param([string]$LiteralPath, [string]$Algorithm) [pscustomobject]@{ Hash = $env:FAKE_INSTALLER_SHA } }
function Invoke-CrabboxSetupProgram {
  param([string]$FilePath, [string[]]$ArgumentList)
  $script:events.Add("install " + ($ArgumentList -join " "))
  if ($env:FAKE_INSTALL_EXIT -eq "0") {
    New-Item -ItemType Directory -Force -Path (Join-Path $env:ProgramFiles "Git\bin") | Out-Null
    Set-Content -LiteralPath (Join-Path $env:ProgramFiles "Git\bin\bash.exe") -Value "bash"
  }
  [int]$env:FAKE_INSTALL_EXIT
}
`
	run := func(t *testing.T, programFiles, sha, exit string) (string, error) {
		t.Helper()
		script := filepath.Join(t.TempDir(), "gitbash.ps1")
		body := stubs + "try {\n" + windowsRunnerGitBashPowerShell() + "\n} finally {\n$script:events -join \"`n\"\n\"path0=\" + ($env:Path -split ';')[0]\n\"leftovers=\" + @(Get-ChildItem -LiteralPath ([IO.Path]::GetTempPath()) -Filter 'crabbox-git-for-windows-*' -ErrorAction SilentlyContinue).Count\n}\n"
		if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		temp := t.TempDir()
		cmd := exec.Command(ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
		cmd.Env = append(os.Environ(), "FAKE_PROGRAM_FILES="+programFiles, "TEMP="+temp, "TMP="+temp, "FAKE_INSTALLER_SHA="+sha, "FAKE_INSTALL_EXIT="+exit)
		out, err := cmd.CombinedOutput()
		return strings.ReplaceAll(string(out), "\r\n", "\n"), err
	}
	gitBin := func(programFiles string) string { return filepath.Join(programFiles, "Git", "bin") }

	t.Run("installs the pinned Git for Windows when bash is missing", func(t *testing.T) {
		programFiles := t.TempDir()
		out, err := run(t, programFiles, gitForWindowsSetupSHA256, "0")
		if err != nil {
			t.Fatalf("provisioning failed: %v\n%s", err, out)
		}
		for _, want := range []string{"stage git-bash", "download " + gitForWindowsSetupURL, "install /VERYSILENT /NORESTART /SUPPRESSMSGBOXES /NOCANCEL /SP- /o:PathOption=BashOnly", "path0=" + gitBin(programFiles), "leftovers=0"} {
			if !strings.Contains(out, want+"\n") {
				t.Errorf("missing %q in:\n%s", want, out)
			}
		}
	})

	t.Run("reuses an existing Git Bash", func(t *testing.T) {
		programFiles := t.TempDir()
		if err := os.MkdirAll(gitBin(programFiles), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gitBin(programFiles), "bash.exe"), []byte("bash"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := run(t, programFiles, "unused", "0")
		if err != nil || strings.Contains(out, "download ") || strings.Contains(out, "install ") || !strings.Contains(out, "path0="+gitBin(programFiles)+"\n") {
			t.Fatalf("an existing Git Bash must be put on PATH without reinstalling: err=%v\n%s", err, out)
		}
	})

	t.Run("refuses an installer with the wrong checksum", func(t *testing.T) {
		out, err := run(t, t.TempDir(), strings.Repeat("0", 64), "0")
		if err == nil || !strings.Contains(out, "Git for Windows installer checksum mismatch") || strings.Contains(out, "install ") || !strings.Contains(out, "leftovers=0") {
			t.Fatalf("a mismatched installer must not run and must be removed: err=%v\n%s", err, out)
		}
	})

	t.Run("fails when the installer fails", func(t *testing.T) {
		out, err := run(t, t.TempDir(), gitForWindowsSetupSHA256, "1")
		if err == nil || !strings.Contains(out, "Git for Windows installer exited 1") {
			t.Fatalf("a failed install must fail runner setup: err=%v\n%s", err, out)
		}
	})
}

// The Git for Windows installer spawns its setup child and exits with its
// result; runner setup must wait for it and see a failing exit code.
func TestWindowsSetupProgramWaitsAndReturnsExitCode(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("runs Windows PowerShell")
	}
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell is unavailable")
	}
	marker := filepath.Join(t.TempDir(), "done.txt")
	script := windowsSetupProgramPowerShell + `
$code = Invoke-CrabboxSetupProgram -FilePath $env:ComSpec -ArgumentList @("/d", "/c", "ping -n 2 127.0.0.1 >nul & echo done>" + $env:MARKER + " & exit 7")
"exit=" + $code
"marker=" + (Test-Path -LiteralPath $env:MARKER)
`
	cmd := exec.Command(ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "MARKER="+marker)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "exit=7") || !strings.Contains(string(out), "marker=True") {
		t.Fatalf("setup program helper must wait for completion and return its exit code: %v\n%s", err, out)
	}
}
