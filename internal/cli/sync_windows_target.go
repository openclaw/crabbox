package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

func syncWindowsNative(ctx context.Context, target SSHTarget, repo Repo, cfg Config, workdir string, manifest SyncManifest, stdout, stderr anyWriter, opts rsyncOptions) error {
	if err := runSSHQuiet(ctx, target, windowsPrepareWorkdir(workdir, cfg.Sync.Delete)); err != nil {
		return exit(7, "prepare remote workdir: %v", err)
	}
	if cfg.Sync.GitSeed {
		if err := runSSHQuiet(ctx, target, windowsGitSeed(workdir, repo.RemoteURL, repo.Head)); err != nil {
			fmt.Fprintf(stderr, "warning: remote git seed failed: %v\n", err)
		}
	}
	var input bytes.Buffer
	input.Write(manifest.NUL())
	cmd := exec.CommandContext(ctx, "tar", "-czf", "-", "-C", repo.Root, "--null", "-T", "-")
	cmd.Stdin = &input
	var archive bytes.Buffer
	cmd.Stdout = &archive
	cmd.Stderr = stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		return exit(6, "create sync archive: %v", err)
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	stopHeartbeat := startSyncHeartbeat(stderr, start, opts.HeartbeatInterval)
	err := runSSHInput(ctx, target, windowsExtractArchive(workdir), &archive, stdout, stderr)
	stopHeartbeat()
	if ctx.Err() == context.DeadlineExceeded {
		return exit(6, "archive sync timed out after %s", opts.Timeout)
	}
	if err != nil {
		return exit(6, "archive sync failed: %v", err)
	}
	return nil
}

type anyWriter interface {
	Write([]byte) (int, error)
}

func windowsPrepareWorkdir(workdir string, delete bool) string {
	deleteScript := ""
	if delete {
		deleteScript = `
if (Test-Path -LiteralPath $workdir) {
  Get-ChildItem -LiteralPath $workdir -Force | Where-Object { $_.Name -ne '.git' } | Remove-Item -Recurse -Force
}
`
	}
	return powershellCommand(`$ErrorActionPreference = "Stop"
$workdir = ` + psQuote(workdir) + `
New-Item -ItemType Directory -Force -Path $workdir | Out-Null
` + deleteScript)
}

func windowsExtractArchive(workdir string) string {
	return powershellCommand(`$ErrorActionPreference = "Stop"
$workdir = ` + psQuote(workdir) + `
New-Item -ItemType Directory -Force -Path $workdir | Out-Null
tar -xzf - -C $workdir
`)
}

func windowsGitSeed(workdir, remoteURL, head string) string {
	remoteURL = normalizeGitRemoteURL(remoteURL)
	if remoteURL == "" || head == "" {
		return powershellCommand(`exit 0`)
	}
	return powershellCommand(`$ErrorActionPreference = "Stop"
$workdir = ` + psQuote(workdir) + `
$parent = Split-Path -Parent $workdir
New-Item -ItemType Directory -Force -Path $parent | Out-Null
if (-not (Test-Path -LiteralPath (Join-Path $workdir ".git"))) {
  $tmp = Join-Path $parent (".seed-" + [System.Guid]::NewGuid().ToString("N"))
  git clone --quiet --filter=blob:none --no-checkout ` + psQuote(remoteURL) + ` $tmp
  Push-Location $tmp
  git fetch --quiet --depth=1 origin ` + psQuote(head) + ` 2>$null
  git checkout --quiet ` + psQuote(head) + ` 2>$null
  if ($LASTEXITCODE -ne 0) { git checkout --quiet FETCH_HEAD 2>$null }
  Pop-Location
  if (Test-Path -LiteralPath $workdir) {
    Get-ChildItem -LiteralPath $workdir -Force | Remove-Item -Recurse -Force
  } else {
    New-Item -ItemType Directory -Force -Path $workdir | Out-Null
  }
  Get-ChildItem -LiteralPath $tmp -Force | Move-Item -Destination $workdir -Force
  Remove-Item -LiteralPath $tmp -Force
}
`)
}

func windowsRemoteCommandWithEnvFile(workdir string, env map[string]string, envFile string, command []string) string {
	var b bytes.Buffer
	writeWindowsRemotePrefix(&b, workdir, env, envFile)
	if len(command) == 0 {
		b.WriteString("exit 0\n")
	} else {
		b.WriteString("& " + psQuote(command[0]))
		for _, arg := range command[1:] {
			b.WriteByte(' ')
			b.WriteString(psQuote(arg))
		}
		b.WriteString("\nexit $LASTEXITCODE\n")
	}
	return powershellCommand(b.String())
}

func windowsRemoteShellCommandWithEnvFile(workdir string, env map[string]string, envFile, script string) string {
	var b bytes.Buffer
	writeWindowsRemotePrefix(&b, workdir, env, envFile)
	b.WriteString(script)
	b.WriteString("\nif (-not $?) { exit 1 }\n")
	b.WriteString("if ($null -ne $global:LASTEXITCODE) { exit $global:LASTEXITCODE }\n")
	return powershellCommand(b.String())
}

func writeWindowsRemotePrefix(b *bytes.Buffer, workdir string, env map[string]string, envFile string) {
	b.WriteString(`$ErrorActionPreference = "Stop"` + "\n")
	b.WriteString(`Set-Location -LiteralPath ` + psQuote(workdir) + "\n")
	if envFile != "" {
		b.WriteString(`if (Test-Path -LiteralPath ` + psQuote(envFile) + `) { Get-Content -LiteralPath ` + psQuote(envFile) + ` | ForEach-Object { if ($_ -match '^([^=]+)=(.*)$') { [Environment]::SetEnvironmentVariable($matches[1], $matches[2], 'Process') } } }` + "\n")
	}
	for key, value := range env {
		b.WriteString(`$env:` + key + ` = ` + psQuote(value) + "\n")
	}
}

func windowsRemoteMkdir(workdir string) string {
	return powershellCommand(`New-Item -ItemType Directory -Force -Path ` + psQuote(workdir) + ` | Out-Null`)
}

func windowsRemoteReadResultFiles(workdir string, paths []string) string {
	var b bytes.Buffer
	b.WriteString(`$ErrorActionPreference = "SilentlyContinue"` + "\n")
	b.WriteString(`Set-Location -LiteralPath ` + psQuote(workdir) + "\n")
	for _, path := range paths {
		b.WriteString(`$f = ` + psQuote(path) + "\n")
		b.WriteString(`if (Test-Path -LiteralPath $f) { Write-Output "` + resultFileMarker + `${f}"; Get-Content -Raw -LiteralPath $f }` + "\n")
	}
	return powershellCommand(b.String())
}

func windowsRemoteDoctor() string {
	return powershellCommand(`$ErrorActionPreference = "Stop"
Write-Output ("git=" + (git --version))
Write-Output ("tar=" + ((tar --version | Select-Object -First 1) -join ""))
Write-Output ("powershell=" + $PSVersionTable.PSVersion.ToString())
`)
}

func windowsRemoteCacheUnsupported() string {
	return powershellCommand(`Write-Output "cache		native Windows cache commands are not supported"`)
}
