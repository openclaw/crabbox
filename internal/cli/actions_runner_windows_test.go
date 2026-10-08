package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWindowsActionsRunnerUploadUsesFileCopyAndCleansUp(t *testing.T) {
	dir := actionsRunnerFixtureDirectory(t)
	tools := installScriptedSSHExecutable(t, dir, scriptedSSHBehavior{})
	scpLog := filepath.Join(dir, "scp-args")
	sshLog := filepath.Join(dir, "ssh-calls")
	installRecordingTestTool(t, tools, "scp", scpLog)
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEMP", dir)
	t.Setenv("TMP", dir)
	t.Setenv("CRABBOX_FAKE_SSH_LOG", sshLog)
	target := SSHTarget{Host: "fixture.invalid", User: "fixture-user", Port: "22", TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	remotePath, cleanup, err := uploadWindowsActionsRunnerScript(context.Background(), target, githubActionsRunnerInstallPowerShellScript("latest", true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(remotePath, `C:\ProgramData\crabbox\install-runner-`) || !strings.HasSuffix(remotePath, ".ps1") {
		t.Fatalf("unexpected copied script path: %q", remotePath)
	}
	args, err := os.ReadFile(scpLog)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(fields) < 2 || !strings.HasSuffix(fields[len(fields)-2], "install-runner.ps1") {
		t.Fatalf("runner installer was not transferred as a file: %q", args)
	}
	if fields[len(fields)-1] != sshTransportHostAlias+":"+strings.ReplaceAll(remotePath, `\`, "/") {
		t.Fatalf("runner installer was copied to the wrong target: %q", args)
	}
	if _, err := os.Stat(fields[len(fields)-2]); !os.IsNotExist(err) {
		t.Fatalf("local runner installer survived upload: %v", err)
	}
	cleanup()
	calls, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "Remove-Item -Force -LiteralPath "+psQuote(remotePath)) {
		t.Fatalf("remote installer cleanup was not requested through mocked SSH: %q", calls)
	}
}

func TestWindowsActionsRunnerUploadedCommandTransfersInputsAndCleansUp(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell is required")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal(err)
	}
	for _, exitCode := range []int{0, 17} {
		t.Run(fmt.Sprintf("exit-%d", exitCode), func(t *testing.T) {
			dir := actionsRunnerFixtureDirectory(t)
			scriptPath := filepath.Join(dir, "install fixture.ps1")
			script := fmt.Sprintf(`[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
Write-Output (@{
  Repo = $env:RUNNER_REPO; Name = $env:RUNNER_NAME; Labels = $env:RUNNER_LABELS; Token = $env:RUNNER_TOKEN
} | ConvertTo-Json -Compress)
exit %d
`, exitCode)
			if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			remote := `[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
` + decodePowerShellCommand(t, githubActionsRunnerInstallRemoteCommandForUploadedScript(scriptPath))
			const token = "fixture-registration-value"
			cmd := exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", remote)
			cmd.Dir = dir
			cmd.Stdin = strings.NewReader(githubActionsRunnerCredentialInput("example-org/my-app", "runner-λ", "self-hosted,windows", token))
			output, runErr := cmd.CombinedOutput()
			if exitCode == 0 && runErr != nil {
				t.Fatalf("uploaded installer failed: %v\n%s", runErr, output)
			}
			if exitCode != 0 {
				var exitErr *exec.ExitError
				if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != exitCode {
					t.Fatalf("installer exit status changed: %v\n%s", runErr, output)
				}
			}
			var values struct{ Repo, Name, Labels, Token string }
			if err := json.Unmarshal(output, &values); err != nil {
				t.Fatalf("copied installer did not receive the stdin inputs: %v\n%s", err, output)
			}
			if values.Repo != "example-org/my-app" || values.Name != "runner-λ" || values.Labels != "self-hosted,windows" || values.Token != token {
				t.Fatalf("copied installer received incorrect inputs: %+v", values)
			}
			if _, err := os.Stat(scriptPath); !os.IsNotExist(err) {
				t.Fatalf("copied installer survived exit %d: %v", exitCode, err)
			}
			for _, argument := range cmd.Args {
				if strings.Contains(argument, token) {
					t.Fatal("registration input was placed on the SSH command line")
				}
			}
		})
	}
}

const windowsRunnerFixtureHashPowerShell = `
function Get-FileHash {
  param($LiteralPath, $Algorithm)
  if ($Algorithm -ne "SHA256") { throw "unexpected fixture checksum algorithm" }
  $sha = [Security.Cryptography.SHA256]::Create()
  try {
    [PSCustomObject]@{ Hash = ([BitConverter]::ToString($sha.ComputeHash([IO.File]::ReadAllBytes($LiteralPath)))).Replace("-", "") }
  } finally {
    $sha.Dispose()
  }
}
`

func TestWindowsActionsRunnerInstallerRegistersOneTriggerlessTaskWithExactCredential(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell is required")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal(err)
	}
	dir := actionsRunnerFixtureDirectory(t)
	credentialPath := filepath.Join(dir, "windows.password")
	const credential = " \tfixture-λ\r\n "
	if err := os.WriteFile(credentialPath, []byte(credential), 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("fixture-archive"))
	prefix := fmt.Sprintf(`$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
Set-Variable -Name HOME -Value %s -Force
$env:PROCESSOR_ARCHITECTURE = "AMD64"
$env:RUNNER_REPO = "example-org/my-app"
$env:RUNNER_NAME = "runner-1"
$env:RUNNER_LABELS = "self-hosted,windows"
$env:RUNNER_TOKEN = "fixture-registration-value"
$script:starts = 0
$script:registrations = 0
$script:metadata = 0
$script:downloads = 0
function Invoke-RestMethod {
  param($Uri, [switch]$UseBasicParsing, $TimeoutSec)
  if ($Uri -ne "https://api.github.com/repos/actions/runner/releases/tags/v2.3.4") { throw "unexpected metadata URI" }
  $script:metadata++
  @{ tag_name = "v2.3.4"; assets = @(@{ name = "actions-runner-win-x64-2.3.4.zip"; digest = "sha256:%s" }) }
}
function Invoke-WebRequest {
  param($Uri, $OutFile, [switch]$UseBasicParsing, $TimeoutSec)
  if ($Uri -ne "https://github.com/actions/runner/releases/download/v2.3.4/actions-runner-win-x64-2.3.4.zip") { throw "unexpected archive URI" }
  $script:downloads++
  [IO.File]::WriteAllBytes($OutFile, [Text.Encoding]::UTF8.GetBytes("fixture-archive"))
}
function Expand-Archive {
  param($LiteralPath, $DestinationPath, [switch]$Force)
  [IO.File]::WriteAllText((Join-Path $DestinationPath "config.cmd"), "@echo off`+"`r`n"+`echo fixture-configured`+"`r`n"+`exit /b 0`+"`r`n"+`", [Text.Encoding]::ASCII)
}
function Start-Job {
  param($ScriptBlock, $ArgumentList)
  $script:jobOutput = @(& $ScriptBlock @ArgumentList)
  [PSCustomObject]@{ State = "Completed" }
}
function Wait-Job { param($Job, $Timeout) $Job }
function Receive-Job { param($Job, $ErrorAction) $script:jobOutput }
function Remove-Job { param($Job, [switch]$Force, $ErrorAction) }
function whoami { "fixture-user" }
function Unregister-ScheduledTask { param($TaskName, $Confirm, $ErrorAction) }
function New-ScheduledTaskAction {
  param($Execute, $Argument)
  @{ Execute = $Execute; Argument = $Argument }
}
function New-ScheduledTaskSettingsSet {
  param($ExecutionTimeLimit, $MultipleInstances)
  @{ ExecutionTimeLimit = $ExecutionTimeLimit.TotalSeconds; MultipleInstances = $MultipleInstances }
}
function Register-ScheduledTask {
  param($TaskName, $Action, $Settings, $User, $Password, $RunLevel, [switch]$Force, $Trigger)
  $script:registrations++
  $script:registered = @{
    Name = $TaskName; User = $User; Credential = $Password; HasTrigger = $PSBoundParameters.ContainsKey("Trigger")
    Settings = $Settings; Action = $Action; RunLevel = $RunLevel
  }
}
function Start-ScheduledTask {
  param($TaskName)
  if ($TaskName -ne $script:registered.Name) { throw "started the wrong task" }
  $script:starts++
}
`, psQuote(dir), hex.EncodeToString(hash[:]))
	script := prefix + windowsRunnerFixtureHashPowerShell + githubActionsRunnerInstallPowerShellScriptWithCredentialPath("2.3.4", true, credentialPath) + `
Write-Output ("FIXTURE_JSON=" + (@{
  Registration = $script:registered; Registrations = $script:registrations; Starts = $script:starts
  Metadata = $script:metadata; Downloads = $script:downloads
} | ConvertTo-Json -Depth 6 -Compress))
`
	scriptPath := filepath.Join(dir, "install.ps1")
	if err := os.WriteFile(scriptPath, append([]byte{0xef, 0xbb, 0xbf}, []byte(script)...), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TEMP="+dir, "TMP="+dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mocked runner installation failed: %v\n%s", err, output)
	}
	var result struct {
		Registration struct {
			Name, User, Credential, RunLevel string
			HasTrigger                       bool
			Settings                         struct {
				ExecutionTimeLimit float64
				MultipleInstances  string
			}
		}
		Registrations, Starts, Metadata, Downloads int
	}
	var encoded string
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "FIXTURE_JSON=") {
			encoded = strings.TrimPrefix(line, "FIXTURE_JSON=")
		}
	}
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatalf("read fixture result: %v\n%s", err, output)
	}
	if result.Registrations != 1 || result.Starts != 1 || result.Registration.HasTrigger {
		t.Fatalf("runner must be started exactly once without a trigger: %+v", result)
	}
	if result.Registration.Name != "crabbox-actions-runner-runner-1" || result.Registration.User != "fixture-user" || result.Registration.RunLevel != "Highest" {
		t.Fatalf("unexpected task registration: %+v", result.Registration)
	}
	if result.Registration.Credential != credential {
		t.Fatalf("guest credential was changed: got %q", result.Registration.Credential)
	}
	if result.Registration.Settings.MultipleInstances != "IgnoreNew" || result.Registration.Settings.ExecutionTimeLimit != 0 {
		t.Fatalf("runner task settings allow duplicate or bounded execution: %+v", result.Registration.Settings)
	}
	if result.Metadata != 1 || result.Downloads != 1 {
		t.Fatalf("runner did not use the official release metadata and archive: %+v", result)
	}
	configLog, err := os.ReadFile(filepath.Join(dir, "actions-runner", "crabbox-config.log"))
	if err != nil || !strings.Contains(string(configLog), "fixture-configured") {
		t.Fatalf("runner was not configured through config.cmd: %q, %v", configLog, err)
	}
}

func TestWindowsActionsRunnerRejectsInvalidCredentialBeforeTaskMutation(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell is required")
	}

	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"empty", nil, "credential file is empty"},
		{"invalid-utf8", []byte{0xff}, "credential file is not valid UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := actionsRunnerFixtureDirectory(t)
			credentialPath := filepath.Join(dir, "windows.password")
			if err := os.WriteFile(credentialPath, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			script := `$ErrorActionPreference = "Stop"
function Write-CrabboxRunnerStage { param($Stage) }
function Unregister-ScheduledTask { throw "task mutation must not happen" }
function Register-ScheduledTask { throw "task mutation must not happen" }
function Start-Process { throw "process launch must not happen" }
` + githubActionsRunnerStartPowerShell(credentialPath)
			scriptPath := filepath.Join(dir, "start.ps1")
			if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
			cmd.Dir = dir
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), tc.want) || strings.Contains(string(output), "must not happen") {
				t.Fatalf("invalid credential was not rejected before mutation: %v\n%s", err, output)
			}
		})
	}
}

func TestWindowsActionsRunnerChecksumMismatchPreservesExistingInstallation(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell is required")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal(err)
	}
	dir := actionsRunnerFixtureDirectory(t)
	runnerDir := filepath.Join(dir, "actions-runner")
	if err := os.Mkdir(runnerDir, 0700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(runnerDir, "config.cmd")
	const original = "original runner configuration"
	if err := os.WriteFile(configPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf(`$ErrorActionPreference = "Stop"
Set-Variable -Name HOME -Value %s -Force
$env:PROCESSOR_ARCHITECTURE = "AMD64"
$env:RUNNER_REPO = "example-org/my-app"
$env:RUNNER_NAME = "runner-1"
$env:RUNNER_LABELS = "self-hosted,windows"
$env:RUNNER_TOKEN = "fixture-registration-value"
function Invoke-RestMethod {
  param($Uri, [switch]$UseBasicParsing, $TimeoutSec)
  @{ tag_name = "v2.3.4"; assets = @(@{ name = "actions-runner-win-x64-2.3.4.zip"; digest = "sha256:%s" }) }
}
function Invoke-WebRequest {
  param($Uri, $OutFile, [switch]$UseBasicParsing, $TimeoutSec)
  [IO.File]::WriteAllBytes($OutFile, [Text.Encoding]::UTF8.GetBytes("corrupt fixture archive"))
}
function Expand-Archive { throw "must not extract an unverified archive" }
function Register-ScheduledTask { throw "must not register an unverified runner" }
`, psQuote(dir), strings.Repeat("0", 64))
	scriptPath := filepath.Join(dir, "install.ps1")
	script := prefix + windowsRunnerFixtureHashPowerShell + githubActionsRunnerInstallPowerShellScriptWithCredentialPath("2.3.4", true, filepath.Join(dir, "windows.password"))
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TEMP="+dir, "TMP="+dir)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "runner archive checksum mismatch") || strings.Contains(string(output), "must not") {
		t.Fatalf("corrupt runner archive was not rejected: %v\n%s", err, output)
	}
	actual, err := os.ReadFile(configPath)
	if err != nil || string(actual) != original {
		t.Fatalf("existing runner was removed before checksum verification: %q, %v", actual, err)
	}
}
