package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Command environments travel separately from the command, including any
// workspace-owner wrapper. The caller may retain Crabbox-owned metadata inline.
type sshCommandEnv struct {
	File  string
	close func()
}

func stageSSHCommandEnv(ctx context.Context, target SSHTarget, workdir string, env map[string]string, stderr io.Writer) (sshCommandEnv, error) {
	prepared := sshCommandEnv{close: func() {}}
	private := map[string]string{}
	for name, value := range env {
		if !ValidShellEnvName(name) {
			continue
		}
		private[name] = value
	}
	if len(private) == 0 {
		return prepared, nil
	}
	nonce, err := randomHex(16)
	if err != nil {
		return prepared, fmt.Errorf("create command environment name: %w", err)
	}
	dir := ".crabbox/env-" + nonce
	prepared.File = dir + "/values.sh"
	input := formatShellEnvFile(private)
	if isWindowsNativeTarget(target) {
		prepared.File = dir + "/values.ps1"
		input = formatPowerShellCommandEnv(private)
	}
	prepared.close = func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := runSSHQuiet(cleanupCtx, target, removeSSHCommandEnvCommand(target, workdir, prepared.File)); err != nil {
			// Remote output can contain values (for example, shell tracing).
			fmt.Fprintln(stderr, "warning: remote command environment cleanup failed")
		}
	}
	// Install cleanup before upload: a failed SSH call may have written the file.
	if err := runSSHInput(ctx, target, uploadSSHCommandEnvCommand(target, workdir, prepared.File), strings.NewReader(input), io.Discard, io.Discard); err != nil {
		prepared.close()
		return prepared, Exit(7, "upload command environment failed")
	}
	return prepared, nil
}

func formatPowerShellCommandEnv(env map[string]string) string {
	var b strings.Builder
	for _, name := range sortedEnvNames(env) {
		if ValidShellEnvName(name) {
			fmt.Fprintf(&b, "$env:%s = %s\n", name, psQuote(env[name]))
		}
	}
	return b.String()
}

func uploadSSHCommandEnvCommand(target SSHTarget, workdir, path string) string {
	dir := shellDir(path)
	if isWindowsNativeTarget(target) {
		return PowershellCommand(`$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath ` + psQuote(workdir) + `
if (Test-Path -LiteralPath '.crabbox') {
  if ((Get-Item -Force -LiteralPath '.crabbox').Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'unsafe environment directory' }
} else { New-Item -ItemType Directory -Path '.crabbox' | Out-Null }
$dir = ` + psQuote(dir) + `
New-Item -ItemType Directory -Path $dir | Out-Null
$acl = New-Object Security.AccessControl.DirectorySecurity
$acl.SetAccessRuleProtection($true, $false)
$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User
$rule = New-Object Security.AccessControl.FileSystemAccessRule($sid, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')
$acl.AddAccessRule($rule)
Set-Acl -LiteralPath $dir -AclObject $acl
$path = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath(` + psQuote(path) + `)
$file = [IO.File]::Open($path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
try {
  $bom = [byte[]](0xEF, 0xBB, 0xBF)
  $file.Write($bom, 0, $bom.Length)
  [Console]::OpenStandardInput().CopyTo($file)
} finally { $file.Dispose() }
`)
	}
	return remoteHermeticPOSIXControlCommand("set -eu\ncd " + shellPathQuote(workdir) + "\n" +
		"umask 077\ntest ! -L .crabbox\nmkdir -p .crabbox\nmkdir " + shellQuote(dir) + "\n" +
		"set -C\ncat > " + shellQuote(path) + "\n")
}

func removeSSHCommandEnvCommand(target SSHTarget, workdir, path string) string {
	if isWindowsNativeTarget(target) {
		return PowershellCommand(`$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath ` + psQuote(workdir) + `
Remove-Item -Force -LiteralPath ` + psQuote(path) + ` -ErrorAction SilentlyContinue
if (Test-Path -LiteralPath ` + psQuote(shellDir(path)) + `) { [IO.Directory]::Delete($ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath(` + psQuote(shellDir(path)) + `)) }
`)
	}
	return remoteHermeticPOSIXControlCommand("set -eu\ncd " + shellPathQuote(workdir) + "\nrm -f -- " + shellQuote(path) + "\n" +
		"if [ -d " + shellQuote(shellDir(path)) + " ]; then rmdir -- " + shellQuote(shellDir(path)) + "; fi\n")
}

func isSSHCommandEnvFile(path string) bool {
	return strings.HasPrefix(path, ".crabbox/env-") && (strings.HasSuffix(path, "/values.sh") || strings.HasSuffix(path, "/values.ps1"))
}
