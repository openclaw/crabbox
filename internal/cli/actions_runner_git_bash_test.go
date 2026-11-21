package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Workflow steps with `shell: bash` fail on a native Windows runner unless the
// runner can resolve Git Bash. The runner setup reuses an existing Git for
// Windows Bash wherever it is installed, installs the pinned Git for Windows
// only when none exists, verifies it, and puts that bin directory first on the
// runner's PATH.
func TestWindowsRunnerGitBashProvisioning(t *testing.T) {
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell not available")
	}
	// The download, hash, and installer are stubbed; Test-Path, Join-Path and
	// Remove-Item run for real against a temporary ProgramFiles.
	stubs := `$ErrorActionPreference = "Stop"
$env:ProgramFiles = $env:FAKE_PROGRAM_FILES
${env:ProgramFiles(x86)} = $env:FAKE_PROGRAM_FILES_X86
$env:LOCALAPPDATA = $env:FAKE_LOCAL_APP_DATA
$env:Path = $env:FAKE_PATH
$runnerDir = $env:FAKE_RUNNER_DIR
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
	run := func(t *testing.T, programFiles, sha, exit string, env ...string) (string, error) {
		t.Helper()
		script := filepath.Join(t.TempDir(), "gitbash.ps1")
		body := stubs + "try {\n" + windowsRunnerGitBashPowerShell() + "\n} finally {\n$script:events -join \"`n\"\n\"path0=\" + ($env:Path -split ';')[0]\n\"leftovers=\" + @(Get-ChildItem -LiteralPath ([IO.Path]::GetTempPath()) -Filter 'crabbox-git-for-windows-*' -ErrorAction SilentlyContinue).Count\n\"recorded=\" + $(if (Test-Path -LiteralPath (Join-Path $runnerDir '" + windowsRunnerGitBinRecord + "')) { [IO.File]::ReadAllText((Join-Path $runnerDir '" + windowsRunnerGitBinRecord + "')) })\n}\n"
		if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		temp := t.TempDir()
		cmd := exec.Command(ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
		cmd.Env = windowsPowerShellTestEnv(t, "FAKE_PROGRAM_FILES="+programFiles, "TEMP="+temp, "TMP="+temp, "FAKE_INSTALLER_SHA="+sha, "FAKE_INSTALL_EXIT="+exit,
			"FAKE_PROGRAM_FILES_X86="+t.TempDir(), "FAKE_LOCAL_APP_DATA="+t.TempDir(), "FAKE_PATH="+t.TempDir(), "FAKE_RUNNER_DIR="+t.TempDir())
		cmd.Env = append(cmd.Env, env...)
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
		for _, want := range []string{"stage git-bash", "download " + gitForWindowsSetupURL, "install /VERYSILENT /NORESTART /SUPPRESSMSGBOXES /NOCANCEL /SP- /o:PathOption=BashOnly", "path0=" + gitBin(programFiles), "leftovers=0", "recorded=" + gitBin(programFiles)} {
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

	touch := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reuses := func(t *testing.T, want string, env ...string) {
		t.Helper()
		out, err := run(t, t.TempDir(), "unused", "0", env...)
		if err != nil || strings.Contains(out, "download ") || strings.Contains(out, "install ") || !strings.Contains(out, "path0="+want+"\n") || !strings.Contains(out, "recorded="+want+"\n") {
			t.Fatalf("an existing Git Bash at %s must be put on PATH without installing: err=%v\n%s", want, err, out)
		}
	}

	t.Run("reuses a per-user Git Bash", func(t *testing.T) {
		local := t.TempDir()
		touch(t, filepath.Join(local, "Programs", "Git", "bin", "bash.exe"))
		reuses(t, filepath.Join(local, "Programs", "Git", "bin"), "FAKE_LOCAL_APP_DATA="+local)
	})

	t.Run("reuses a portable Git Bash whose git.exe is on PATH", func(t *testing.T) {
		portable := t.TempDir()
		touch(t, filepath.Join(portable, "cmd", "git.exe"))
		touch(t, filepath.Join(portable, "bin", "bash.exe"))
		reuses(t, filepath.Join(portable, "bin"), "FAKE_PATH="+filepath.Join(portable, "cmd"))
	})

	t.Run("does not mistake MinGit or the WSL bash launcher for Git Bash", func(t *testing.T) {
		minGit, system32 := t.TempDir(), t.TempDir()
		touch(t, filepath.Join(minGit, "cmd", "git.exe"))
		touch(t, filepath.Join(system32, "bash.exe"))
		programFiles := t.TempDir()
		out, err := run(t, programFiles, gitForWindowsSetupSHA256, "0", "FAKE_PATH="+filepath.Join(minGit, "cmd")+";"+system32)
		if err != nil || !strings.Contains(out, "stage git-bash\n") || !strings.Contains(out, "path0="+gitBin(programFiles)+"\n") {
			t.Fatalf("MinGit and the WSL launcher must not stand in for Git Bash: err=%v\n%s", err, out)
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

// The listener task runs run-crabbox.ps1, so the runner process finds the Git
// Bash that setup recorded, including per-user and portable installations; a
// runner configured before the record existed keeps the Program Files Bash.
func TestWindowsRunnerListenerRunScriptPutsGitBashFirst(t *testing.T) {
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("Windows PowerShell not available")
	}
	touch := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := t.TempDir()
	run := func(t *testing.T, programFiles, recorded string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "run-crabbox.ps1"), []byte(windowsRunnerListenerRunScript()), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "run.cmd"), []byte("@echo path=%PATH%\r\n@exit /b 7\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if recorded != "" {
			if err := os.WriteFile(filepath.Join(dir, windowsRunnerGitBinRecord), []byte(recorded), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		// Like the setup test, pin Program Files and PATH inside PowerShell so the
		// host's own Git for Windows cannot leak in.
		wrapper := filepath.Join(dir, "wrapper.ps1")
		if err := os.WriteFile(wrapper, []byte(`$env:ProgramFiles = $env:FAKE_PROGRAM_FILES
$env:Path = $env:FAKE_PATH
& (Join-Path $PSScriptRoot "run-crabbox.ps1")
exit $LASTEXITCODE
`), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", wrapper)
		cmd.Env = windowsPowerShellTestEnv(t, "FAKE_PROGRAM_FILES="+programFiles, "FAKE_PATH="+base)
		out, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
			t.Fatalf("run.cmd exit status must pass through: err=%v\n%s", err, out)
		}
		return strings.ReplaceAll(string(out), "\r\n", "\n")
	}

	t.Run("recorded per-user Git Bash goes first", func(t *testing.T) {
		bin := filepath.Join(t.TempDir(), "Programs", "Git", "bin")
		touch(t, filepath.Join(bin, "bash.exe"))
		if out := run(t, t.TempDir(), bin); !strings.Contains(out, "path="+bin+";"+base+"\n") {
			t.Fatalf("listener PATH must start with %s:\n%s", bin, out)
		}
	})

	t.Run("runner without a record keeps the Program Files Git Bash", func(t *testing.T) {
		programFiles := t.TempDir()
		bin := filepath.Join(programFiles, "Git", "bin")
		touch(t, filepath.Join(bin, "bash.exe"))
		if out := run(t, programFiles, ""); !strings.Contains(out, "path="+bin+";"+base+"\n") {
			t.Fatalf("listener PATH must start with %s:\n%s", bin, out)
		}
	})

	t.Run("PATH is unchanged without Git Bash", func(t *testing.T) {
		if out := run(t, t.TempDir(), ""); !strings.Contains(out, "path="+base+"\n") {
			t.Fatalf("listener PATH changed without Git Bash:\n%s", out)
		}
	})
}

// Windows OpenSSH runs a remote command through cmd.exe, which rejects command
// lines of 8191 characters or more, so the native listener could never start.
func TestWindowsRunnerListenerStartCommandFitsCmdLimit(t *testing.T) {
	command := githubActionsRunnerStartScript(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal})
	if len(command) >= 8191 {
		t.Fatalf("native Windows listener start command is %d characters; cmd.exe accepts fewer than 8191", len(command))
	}
}
