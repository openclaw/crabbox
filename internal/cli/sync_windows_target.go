package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

func syncWindowsNative(ctx context.Context, target SSHTarget, repo Repo, cfg Config, workdir string, manifest SyncManifest, stdout, stderr anyWriter, opts rsyncOptions) error {
	if err := runSSHQuiet(ctx, target, windowsPrepareWorkdir(workdir, cfg.Sync.Delete)); err != nil {
		return exit(7, "prepare remote workdir: %v", err)
	}
	gitSeed := syncGitSeedEnabled(cfg, repo)
	if gitSeed {
		if err := runSSHQuiet(ctx, target, windowsGitSeed(workdir, repo.RemoteURL, repo.Head)); err != nil {
			fmt.Fprintf(stderr, "warning: remote git seed failed: %v\n", err)
		}
	}
	if opts.FullResync && gitSeed {
		// Git seed restores HEAD; full resync must remove paths absent locally before overlay.
		manifestData := manifest.NUL()
		manifestInput := fmt.Sprintf("%d\n", len(manifestData)) + string(manifestData) + string(manifest.DeletedNUL())
		if err := runSSHInputQuiet(ctx, target, windowsPruneSeededSyncManifest(workdir), manifestInput); err != nil {
			return exit(6, "prune seeded Windows sync paths: %v", err)
		}
	}
	archive, err := CreateSyncArchive(ctx, repo, manifest, "crabbox-windows-sync-*.tgz")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	start := time.Now()
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	stopHeartbeat := startSyncHeartbeat(stderr, start, opts.HeartbeatInterval)
	err = runSSHInput(ctx, target, windowsExtractArchive(workdir), archive, stdout, stderr)
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

func windowsPruneSeededSyncManifest(workdir string) string {
	return powershellCommand(`$ErrorActionPreference = "Stop"
$workdir = ` + psQuote(workdir) + `
if (-not (Test-Path -LiteralPath (Join-Path $workdir ".git"))) { exit 0 }
Set-Location -LiteralPath $workdir
$stdin = [Console]::OpenStandardInput()
$buffer = [System.IO.MemoryStream]::new()
$stdin.CopyTo($buffer)
$bytes = $buffer.ToArray()
$newline = [Array]::IndexOf($bytes, [byte]10)
if ($newline -lt 0) { throw "missing manifest header" }
$manifestLen = [int]([System.Text.Encoding]::ASCII.GetString($bytes, 0, $newline))
$manifestBytes = [byte[]]::new($manifestLen)
[Array]::Copy($bytes, $newline + 1, $manifestBytes, 0, $manifestLen)
$deletedLen = $bytes.Length - ($newline + 1 + $manifestLen)
$deletedBytes = [byte[]]::new($deletedLen)
if ($deletedLen -gt 0) {
  [Array]::Copy($bytes, $newline + 1 + $manifestLen, $deletedBytes, 0, $deletedLen)
}
function Read-NulList([byte[]]$data) {
  $set = @{}
  foreach ($rel in ([System.Text.Encoding]::UTF8.GetString($data) -split "` + "`0" + `")) {
    $rel = $rel.Replace("\", "/")
    if ($rel.Length -gt 0) { $set[$rel] = $true }
  }
  return $set
}
$wanted = Read-NulList $manifestBytes
$deleted = Read-NulList $deletedBytes
$root = [System.IO.Path]::GetFullPath($workdir).TrimEnd([char[]]@('\', '/'))
$sep = [string][System.IO.Path]::DirectorySeparatorChar
function Remove-SafeRepoPath([string]$rel) {
  $rel = $rel.Replace("\", "/")
  if ($rel.Length -eq 0 -or [System.IO.Path]::IsPathRooted($rel) -or $rel -eq ".." -or $rel.StartsWith("../") -or $rel.Contains("/../")) { return }
  $full = [System.IO.Path]::GetFullPath([System.IO.Path]::Combine($root, $rel.Replace("/", $sep)))
  if (-not $full.StartsWith($root + $sep, [System.StringComparison]::OrdinalIgnoreCase)) { return }
  Remove-Item -LiteralPath $full -Force -ErrorAction SilentlyContinue
  $dir = Split-Path -Parent $full
  while ($dir -and $dir.StartsWith($root + $sep, [System.StringComparison]::OrdinalIgnoreCase)) {
    try {
      Remove-Item -LiteralPath $dir -Force -ErrorAction Stop
    } catch {
      break
    }
    $dir = Split-Path -Parent $dir
  }
}
foreach ($rel in (& git -c core.quotePath=false ls-files)) {
  $rel = $rel.Replace("\", "/")
  if (-not $wanted.ContainsKey($rel) -or $deleted.ContainsKey($rel)) {
    Remove-SafeRepoPath $rel
  }
}
`)
}

func windowsRemoteCommandWithEnvFile(workdir string, env map[string]string, envFile string, command []string) string {
	return windowsRemoteCommandWithEnvFiles(workdir, env, singleEnvFile(envFile), command)
}

func windowsRemoteCommandWithEnvFiles(workdir string, env map[string]string, envFiles []string, command []string) string {
	var b bytes.Buffer
	writeWindowsRemotePrefix(&b, workdir, env, envFiles)
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
	return windowsRemoteShellCommandWithEnvFiles(workdir, env, singleEnvFile(envFile), script)
}

func windowsRemoteShellCommandWithEnvFiles(workdir string, env map[string]string, envFiles []string, script string) string {
	var b bytes.Buffer
	writeWindowsRemotePrefix(&b, workdir, env, envFiles)
	b.WriteString(script)
	b.WriteString("\nif (-not $?) { exit 1 }\n")
	b.WriteString("if ($null -ne $global:LASTEXITCODE) { exit $global:LASTEXITCODE }\n")
	return powershellCommand(b.String())
}

func writeWindowsRemotePrefix(b *bytes.Buffer, workdir string, env map[string]string, envFiles []string) {
	b.WriteString(`$ErrorActionPreference = "Stop"` + "\n")
	b.WriteString(`Set-Location -LiteralPath ` + psQuote(workdir) + "\n")
	if len(envFiles) > 0 {
		b.WriteString(`function Import-CrabboxEnvFile($Path) {
  if ($Path -match '^/([A-Za-z])/(.*)$') {
    $Path = ($matches[1].ToUpperInvariant() + ':\' + $matches[2].Replace('/', '\'))
  }
  if (-not (Test-Path -LiteralPath $Path)) { return }
  Get-Content -Encoding UTF8 -LiteralPath $Path | ForEach-Object {
    if ($_ -match '^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$') {
      $name = $matches[1]
      $value = $matches[2].Trim()
      if (($value.StartsWith("'") -and $value.EndsWith("'")) -or ($value.StartsWith('"') -and $value.EndsWith('"'))) {
        $value = $value.Substring(1, $value.Length - 2)
      }
      $value = $value.Replace('\ ', ' ')
      [Environment]::SetEnvironmentVariable($name, $value, 'Process')
    }
  }
}
function Add-CrabboxPath($Path) {
  if ([string]::IsNullOrWhiteSpace($Path)) { return }
  if (Test-Path -LiteralPath $Path) { $env:Path = "$Path;$env:Path" }
}
`)
	}
	for _, envFile := range envFiles {
		envFile = strings.TrimSpace(envFile)
		if envFile == "" {
			continue
		}
		b.WriteString(`Import-CrabboxEnvFile ` + psQuote(envFile) + "\n")
	}
	if len(envFiles) > 0 {
		b.WriteString(`Add-CrabboxPath $env:PNPM_HOME
if (-not [string]::IsNullOrWhiteSpace($env:RUNNER_TOOL_CACHE)) {
  $nodeRoot = Join-Path $env:RUNNER_TOOL_CACHE 'node'
  if (Test-Path -LiteralPath $nodeRoot) {
    $node = Get-ChildItem -LiteralPath $nodeRoot -Recurse -Filter node.exe -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($node) { Add-CrabboxPath $node.DirectoryName }
  }
}
`)
	}
	for key, value := range env {
		b.WriteString(`$env:` + key + ` = ` + psQuote(value) + "\n")
	}
}

func windowsRemoteMkdir(workdir string) string {
	return powershellCommand(`New-Item -ItemType Directory -Force -Path ` + psQuote(workdir) + ` | Out-Null`)
}

func windowsRemoteResetWorkdir(workdir string) string {
	return powershellCommand(`$ErrorActionPreference = "Stop"
$workdir = ` + psQuote(workdir) + `
if (Test-Path -LiteralPath $workdir) {
  Remove-Item -LiteralPath $workdir -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $workdir | Out-Null
`)
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

func windowsRemoteTouchResultsMarker(workdir string) string {
	return powershellCommand(`$ErrorActionPreference = "Stop"
Set-Location -LiteralPath ` + psQuote(workdir) + `
` + windowsResolveResultsMarker() + `
$markerDir = Split-Path -Parent $marker
if ($markerDir) { New-Item -ItemType Directory -Force -Path $markerDir | Out-Null }
Set-Content -LiteralPath $marker -Value ""
`)
}

func windowsRemoteFindJUnitResultFiles(workdir, marker string) string {
	var b bytes.Buffer
	b.WriteString(`$ErrorActionPreference = "Stop"` + "\n")
	b.WriteString(`Set-Location -LiteralPath ` + psQuote(workdir) + "\n")
	b.WriteString(`$ErrorActionPreference = "SilentlyContinue"` + "\n")
	b.WriteString(fmt.Sprintf("$maxBytes = %d\n", autoJUnitMaxBytes))
	b.WriteString(fmt.Sprintf("$maxTotalBytes = %d\n", autoJUnitMaxTotalBytes))
	b.WriteString(fmt.Sprintf("$sniffBytes = %d\n", autoJUnitSniffBytes))
	b.WriteString(fmt.Sprintf("$failureSniffBytes = %d\n", autoJUnitFailureSniffBytes))
	b.WriteString(fmt.Sprintf("$maxFiles = %d\n", autoJUnitMaxFiles))
	if strings.TrimSpace(marker) != "" {
		b.WriteString(windowsResolveResultsMarker())
		b.WriteString("\n")
		b.WriteString(`if (-not (Test-Path -LiteralPath $marker)) { return }` + "\n")
		b.WriteString(`$markerTime = (Get-Item -LiteralPath $marker).LastWriteTimeUtc` + "\n")
	}
	b.WriteString(`function Get-CrabboxJUnitFiles([string]$Path, [int]$Depth) {` + "\n")
	b.WriteString(`  if ($Depth -lt 0) { return }` + "\n")
	b.WriteString(`  Get-ChildItem -LiteralPath $Path -Force | ForEach-Object {` + "\n")
	b.WriteString(`    if ($_.PSIsContainer) {` + "\n")
	b.WriteString(`      if ($_.Name -ne 'node_modules' -and $_.Name -ne '.git') { Get-CrabboxJUnitFiles $_.FullName ($Depth - 1) }` + "\n")
	b.WriteString(`    } elseif ($_.Name -like 'junit*.xml' -or $_.Name -like 'TEST-*.xml' -or $_.Name -eq 'results.xml') {` + "\n")
	if strings.TrimSpace(marker) != "" {
		b.WriteString(`      if ($_.LastWriteTimeUtc -ge $markerTime) { $_ }` + "\n")
	} else {
		b.WriteString(`      $_` + "\n")
	}
	b.WriteString(`    }` + "\n")
	b.WriteString(`  }` + "\n")
	b.WriteString(`}` + "\n")
	b.WriteString(`$count = 0` + "\n")
	b.WriteString(`$totalBytes = 0` + "\n")
	b.WriteString(`$files = @(Get-CrabboxJUnitFiles (Get-Location).Path 5 | Sort-Object FullName)` + "\n")
	b.WriteString(`foreach ($wantFailed in @($true, $false)) {` + "\n")
	b.WriteString(`  foreach ($file in $files) {` + "\n")
	b.WriteString(`    if ($count -ge $maxFiles) { break }` + "\n")
	b.WriteString(`    $fs = [System.IO.File]::OpenRead($file.FullName)` + "\n")
	b.WriteString(`    try {` + "\n")
	b.WriteString(`      $sniffLength = [Math]::Min([int64]$sniffBytes, $fs.Length)` + "\n")
	b.WriteString(`      $sniff = New-Object byte[] ([int]$sniffLength)` + "\n")
	b.WriteString(`      $sniffRead = $fs.Read($sniff, 0, $sniff.Length)` + "\n")
	b.WriteString(`      $prefix = if ($sniffRead -gt 0) { [System.Text.Encoding]::UTF8.GetString($sniff, 0, $sniffRead) } else { "" }` + "\n")
	b.WriteString(`      if ($prefix -notmatch '<testsuites?') { continue }` + "\n")
	b.WriteString(`      $fs.Seek(0, [System.IO.SeekOrigin]::Begin) | Out-Null` + "\n")
	b.WriteString(`      $length = [Math]::Min([int64]$failureSniffBytes, $fs.Length)` + "\n")
	b.WriteString(`      $buffer = New-Object byte[] ([int]$length)` + "\n")
	b.WriteString(`      $read = $fs.Read($buffer, 0, $buffer.Length)` + "\n")
	b.WriteString(`      $body = if ($read -gt 0) { [System.Text.Encoding]::UTF8.GetString($buffer, 0, $read) } else { "" }` + "\n")
	b.WriteString(`      $hasFailed = $body -match '<(failure|error)(\s|>)'` + "\n")
	b.WriteString(`      if ($hasFailed -ne $wantFailed) { continue }` + "\n")
	b.WriteString(`      $count++` + "\n")
	b.WriteString(`      if ($fs.Length -gt $maxBytes) { Write-Output "` + resultWarningMarker + `$($file.FullName)` + "`t" + `report exceeds $maxBytes-byte per-file limit"; continue }` + "\n")
	b.WriteString(`      if (($totalBytes + $fs.Length) -gt $maxTotalBytes) { Write-Output "` + resultWarningMarker + `$($file.FullName)` + "`t" + `report exceeds remaining $maxTotalBytes-byte aggregate limit"; continue }` + "\n")
	b.WriteString(`      $totalBytes += $fs.Length` + "\n")
	b.WriteString(`      $fs.Seek(0, [System.IO.SeekOrigin]::Begin) | Out-Null` + "\n")
	b.WriteString(`      $buffer = New-Object byte[] ([int]$fs.Length)` + "\n")
	b.WriteString(`      $read = $fs.Read($buffer, 0, $buffer.Length)` + "\n")
	b.WriteString(`      $body = if ($read -gt 0) { [System.Text.Encoding]::UTF8.GetString($buffer, 0, $read) } else { "" }` + "\n")
	b.WriteString(`      Write-Output "` + resultFileMarker + `$($file.FullName)"` + "\n")
	b.WriteString(`      [Console]::Write($body)` + "\n")
	b.WriteString(`      [Console]::WriteLine()` + "\n")
	b.WriteString(`    } finally {` + "\n")
	b.WriteString(`      $fs.Dispose()` + "\n")
	b.WriteString(`    }` + "\n")
	b.WriteString(`  }` + "\n")
	b.WriteString(`  if ($count -ge $maxFiles) { break }` + "\n")
	b.WriteString(`}` + "\n")
	return powershellCommand(b.String())
}

func windowsResolveResultsMarker() string {
	return `$marker = '.crabbox/results-start'
if (Get-Command git -ErrorAction SilentlyContinue) {
  $gitMarker = & git rev-parse --git-path ` + psQuote(remoteResultsMarker) + ` 2>$null
  if ($LASTEXITCODE -eq 0 -and $gitMarker) { $marker = ([string]$gitMarker).Trim() }
}`
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
