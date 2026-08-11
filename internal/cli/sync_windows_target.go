package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

func syncWindowsNative(ctx context.Context, target SSHTarget, repo Repo, cfg Config, coherence gitCoherencePlan, workdir string, manifest SyncManifest, stdout, stderr anyWriter, opts rsyncOptions) error {
	if err := runSSHQuiet(ctx, target, windowsPrepareWorkdir(workdir, cfg.Sync.Delete)); err != nil {
		return exit(7, "prepare remote workdir: %v", err)
	}
	if coherence.seedEnabled() {
		if err := runSSHQuiet(ctx, target, windowsGitSeed(workdir, coherence)); err != nil {
			fmt.Fprintf(stderr, "warning: remote git seed failed: %v\n", err)
		}
	}
	if opts.FullResync && coherence.seedEnabled() {
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
	if coherence.enabled() {
		if err := runSSHQuiet(ctx, target, windowsGitCoherence(workdir, coherence)); err != nil {
			return exit(6, "align remote Git metadata: %v", err)
		}
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

// Git can report a long path for an 8.3 workdir alias. Compare handle-canonical
// paths so aliases match without accepting an ancestor or sibling repository.
func windowsGitRootIdentityScript() string {
	return `Add-Type -Name GitRootIdentity -Namespace Cbx -MemberDefinition '[DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)]public static extern uint GetFinalPathNameByHandle(Microsoft.Win32.SafeHandles.SafeFileHandle h,System.Text.StringBuilder p,uint n,uint f);[DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)]public static extern Microsoft.Win32.SafeHandles.SafeFileHandle CreateFile(string p,uint a,System.IO.FileShare s,System.IntPtr z,System.IO.FileMode m,uint f,System.IntPtr t);'
function Get-CrabboxFinalDirectoryPath([string]$Path) {
  $share = [IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete
  $handle = [Cbx.GitRootIdentity]::CreateFile($Path, 0, $share, [IntPtr]::Zero, [IO.FileMode]::Open, 0x02000000, [IntPtr]::Zero)
  if ($handle.IsInvalid) { throw "directory identity open failed" }
  try {
    $buffer = New-Object Text.StringBuilder 32768
    if (-not [Cbx.GitRootIdentity]::GetFinalPathNameByHandle($handle, $buffer, $buffer.Capacity, 0)) { throw "directory identity lookup failed" }
    $finalPath = $buffer.ToString()
  } finally {
    $handle.Dispose()
  }
  if ($finalPath.StartsWith('\\?\UNC\')) { $finalPath = '\\' + $finalPath.Substring(8) }
  elseif ($finalPath.StartsWith('\\?\')) { $finalPath = $finalPath.Substring(4) }
  return $finalPath.TrimEnd('\')
}
function Test-CrabboxSameDirectory([string]$Left, [string]$Right) {
  try {
    $leftPath = Get-CrabboxFinalDirectoryPath $Left
    $rightPath = Get-CrabboxFinalDirectoryPath $Right
  } catch {
    return $false
  }
  return [string]::Equals($leftPath, $rightPath, [StringComparison]::OrdinalIgnoreCase)
}
`
}

func windowsGitSeed(workdir string, plan gitCoherencePlan) string {
	if !plan.seedEnabled() {
		return powershellCommand(`exit 0`)
	}
	return powershellCommand(`$ErrorActionPreference = "Stop"
$workdir = ` + psQuote(workdir) + `
$expectedOrigin = ` + psQuote(plan.RemoteURL) + `
$expectedTree = ` + psQuote(plan.Tree) + `
$parent = Split-Path -Parent $workdir
New-Item -ItemType Directory -Force -Path $parent | Out-Null
` + windowsGitRootIdentityScript() + `
function Test-ExactGitRoot([string]$Path) {
  $ErrorActionPreference = "Continue"
  if (-not (Test-Path -LiteralPath $Path -PathType Container)) { return $false }
  $reportedRoot = & git -C $Path rev-parse --show-toplevel 2>$null
  $gitStatus = $LASTEXITCODE
  if ($gitStatus -ne 0 -or -not $reportedRoot) { return $false }
  $reportedRoot = ([string]$reportedRoot).Trim()
  return (Test-CrabboxSameDirectory $Path $reportedRoot)
}
function Test-UsableGitWorkspace([string]$Path) {
  $ErrorActionPreference = "Continue"
  if (-not (Test-ExactGitRoot $Path)) { return $false }
  $head = & git -C $Path rev-parse --verify 'HEAD^{commit}' 2>$null
  $headStatus = $LASTEXITCODE
  if ($headStatus -ne 0 -or -not $head) { return $false }
  $index = & git -C $Path rev-parse --git-path index 2>$null
  $indexStatus = $LASTEXITCODE
  if ($indexStatus -ne 0 -or -not $index) { return $false }
  $index = ([string]$index).Trim()
  if (-not [IO.Path]::IsPathRooted($index)) { $index = [IO.Path]::GetFullPath((Join-Path $Path $index)) }
  if (-not (Test-Path -LiteralPath $index -PathType Leaf)) { return $false }
  $tree = & git -C $Path write-tree 2>$null
  $treeStatus = $LASTEXITCODE
  return $treeStatus -eq 0 -and [bool]$tree
}
function Repair-Origin([string]$Path) {
  $ErrorActionPreference = "Continue"
  & git -C $Path remote get-url origin 2>$null | Out-Null
  $originExists = $LASTEXITCODE -eq 0
  if ($originExists) {
    & git -C $Path remote set-url origin $expectedOrigin 2>$null
  } else {
    & git -C $Path remote add origin $expectedOrigin 2>$null
  }
  if ($LASTEXITCODE -ne 0) { throw "Git origin repair failed" }
  $actualOrigin = & git -C $Path remote get-url origin 2>$null
  if ($LASTEXITCODE -ne 0 -or ([string]$actualOrigin).Trim() -ne $expectedOrigin) { throw "Git origin verification failed" }
}
if (Test-UsableGitWorkspace $workdir) {
  Repair-Origin $workdir
  exit 0
}
$tmp = Join-Path $parent (".seed-" + [System.Guid]::NewGuid().ToString("N"))
try {
  & git clone --quiet --filter=blob:none --no-checkout --single-branch --branch ` + psQuote(plan.Branch) + ` $expectedOrigin $tmp
  if ($LASTEXITCODE -ne 0) { throw "Git seed clone failed" }
  & git -C $tmp checkout --quiet --detach ` + psQuote(plan.Target) + `
  if ($LASTEXITCODE -ne 0) { throw "Git seed checkout failed" }
  $seedHead = & git -C $tmp rev-parse --verify 'HEAD^{commit}' 2>$null
  if ($LASTEXITCODE -ne 0 -or ([string]$seedHead).Trim() -ne ` + psQuote(plan.Target) + `) { throw "Git seed verification failed" }
  if (-not (Test-UsableGitWorkspace $tmp)) { throw "Git seed workspace verification failed" }
  if ($expectedTree) {
    $seedTree = & git -C $tmp write-tree 2>$null
    if ($LASTEXITCODE -ne 0 -or ([string]$seedTree).Trim() -ne $expectedTree) { throw "Git seed tree verification failed" }
  }
  Repair-Origin $tmp
  if (Test-Path -LiteralPath $workdir) {
    Remove-Item -LiteralPath $workdir -Recurse -Force
  }
  Move-Item -LiteralPath $tmp -Destination $workdir
  $tmp = $null
} finally {
  if ($tmp -and (Test-Path -LiteralPath $tmp)) {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }
}
`)
}

func windowsGitCoherence(workdir string, plan gitCoherencePlan) string {
	if !plan.enabled() {
		return powershellCommand(`exit 0`)
	}
	return powershellCommand(`$ErrorActionPreference = "Stop"
$workdir = ` + psQuote(workdir) + `; Set-Location -LiteralPath $workdir
if (-not (Get-Command git -ErrorAction SilentlyContinue)) { exit 0 }
` + windowsGitRootIdentityScript() + `
$reportedRoot = & git rev-parse --show-toplevel 2>$null
if ($LASTEXITCODE -ne 0 -or -not $reportedRoot) { exit 0 }
$reportedRoot = ([string]$reportedRoot).Trim()
if (-not (Test-CrabboxSameDirectory $workdir $reportedRoot)) { exit 0 }
$expectedOrigin = ` + psQuote(plan.RemoteURL) + `
& git remote get-url origin 2>$null | Out-Null
$originExists = $LASTEXITCODE -eq 0
if ($originExists) {
  & git remote set-url origin $expectedOrigin 2>$null
} else {
  & git remote add origin $expectedOrigin 2>$null
}
if ($LASTEXITCODE -ne 0) { throw "Git origin repair failed" }
$actualOrigin = & git remote get-url origin 2>$null
if ($LASTEXITCODE -ne 0 -or ([string]$actualOrigin).Trim() -ne $expectedOrigin) { throw "Git origin verification failed" }
$target = ` + psQuote(plan.Target) + `; $tree = ` + psQuote(plan.Tree) + `; $tmpRef = "refs/crabbox/sync-" + [Guid]::NewGuid().ToString("N")
& git fetch --quiet --no-tags $expectedOrigin ("+refs/heads/" + ` + psQuote(plan.Branch) + ` + ":" + $tmpRef) 2>$null
if ($LASTEXITCODE -ne 0) { & git update-ref -d $tmpRef 2>$null; $null = $LASTEXITCODE; throw "Git coherence fetch failed" }
& git merge-base --is-ancestor $target $tmpRef 2>$null
$isAncestor = $LASTEXITCODE -eq 0
if (-not $isAncestor) { & git update-ref -d $tmpRef 2>$null; $null = $LASTEXITCODE; throw "requested commit is not on advertised branch" }
$targetTree = & git rev-parse --verify ($target + "^{tree}") 2>$null
if ($LASTEXITCODE -ne 0 -or ([string]$targetTree).Trim() -ne $tree) { & git update-ref -d $tmpRef 2>$null; $null = $LASTEXITCODE; throw "requested Git tree verification failed" }
$oldHead = & git rev-parse --verify 'HEAD^{commit}' 2>$null
if ($LASTEXITCODE -ne 0 -or -not $oldHead) { throw "Git HEAD inspection failed" }
$oldHead = ([string]$oldHead).Trim()
$oldSymOutput = & git symbolic-ref -q HEAD 2>$null
$oldSymStatus = $LASTEXITCODE
if ($oldSymStatus -ne 0 -and $oldSymStatus -ne 1) { throw "Git symbolic HEAD inspection failed" }
$oldSym = if ($oldSymStatus -eq 0) { ([string]$oldSymOutput).Trim() } else { $null }
$index = & git rev-parse --git-path index 2>$null
if ($LASTEXITCODE -ne 0 -or -not $index) { throw "Git index path inspection failed" }
$index = ([string]$index).Trim(); if (-not [IO.Path]::IsPathRooted($index)) { $index = [IO.Path]::GetFullPath((Join-Path $workdir $index)) }
$lock = $index + ".lock"; $marker = [Guid]::NewGuid().ToString("N"); $compare = $index + ".crabbox." + $marker + ".compare"; $candidate = $index + ".crabbox." + $marker + ".new"
$verify = $index + ".crabbox." + $marker + ".verify"; $replaceBackup = $index + ".crabbox." + $marker + ".original"; $rollbackBackup = $index + ".crabbox." + $marker + ".rollback"
$indexChanged = $false; $headChanged = $false
try {
  [IO.File]::Copy($index, $compare, $true)
  & git read-tree --reset ("--index-output=" + $candidate) $target
  if ($LASTEXITCODE -ne 0) { throw "target index creation failed" }
  $env:GIT_INDEX_FILE = $candidate
  $candidateTree = & git write-tree 2>$null
  if ($LASTEXITCODE -ne 0 -or ([string]$candidateTree).Trim() -ne $tree) { throw "target index verification failed" }
  Remove-Item Env:GIT_INDEX_FILE -ErrorAction SilentlyContinue
  $stream = [IO.File]::Open($lock, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
  try { $bytes = [Text.Encoding]::ASCII.GetBytes($marker); $stream.Write($bytes, 0, $bytes.Length) } finally { $stream.Dispose() }
  if (-not [Linq.Enumerable]::SequenceEqual([IO.File]::ReadAllBytes($index), [IO.File]::ReadAllBytes($compare))) { throw "Git index changed concurrently" }
  [IO.File]::Copy($candidate, $verify, $true); [IO.File]::Replace($candidate, $index, $replaceBackup); $indexChanged = $true; $env:GIT_INDEX_FILE = $verify
  if (-not [Linq.Enumerable]::SequenceEqual([IO.File]::ReadAllBytes($replaceBackup), [IO.File]::ReadAllBytes($compare))) { throw "Git index changed during replacement" }
  if (-not [Linq.Enumerable]::SequenceEqual([IO.File]::ReadAllBytes($index), [IO.File]::ReadAllBytes($verify))) { throw "installed index bytes changed" }
  $installedTree = & git write-tree 2>$null
  if ($LASTEXITCODE -ne 0 -or ([string]$installedTree).Trim() -ne $tree) { throw "installed index verification failed" }
  Remove-Item Env:GIT_INDEX_FILE -ErrorAction SilentlyContinue
  & git update-ref --no-deref HEAD $target $oldHead
  if ($LASTEXITCODE -ne 0) { throw "HEAD compare-and-swap failed" }; $headChanged = $true
  $installedHead = & git rev-parse --verify 'HEAD^{commit}' 2>$null
  if ($LASTEXITCODE -ne 0 -or ([string]$installedHead).Trim() -ne $target) { throw "Git coherence verification failed" }
  if ($oldSym) {
    $installedSym = & git symbolic-ref -q HEAD 2>$null
    if ($LASTEXITCODE -eq 0 -or -not [string]::IsNullOrWhiteSpace([string]$installedSym)) { throw "Git HEAD remained symbolic during coherence" }
    $preservedBranch = & git rev-parse --verify ($oldSym + '^{commit}') 2>$null
    if ($LASTEXITCODE -ne 0 -or ([string]$preservedBranch).Trim() -ne $oldHead) { throw "Git symbolic branch changed during coherence" }
  }
} catch {
  $failure = $_
  $rollbackError = $null
  Remove-Item Env:GIT_INDEX_FILE -ErrorAction SilentlyContinue
  if ($headChanged) {
    & git update-ref --no-deref HEAD $oldHead $target 2>$null
    if ($LASTEXITCODE -ne 0) {
      $rollbackError = [InvalidOperationException]::new("Git HEAD rollback failed")
    } elseif ($oldSym) {
      $preservedBranch = & git rev-parse --verify ($oldSym + '^{commit}') 2>$null
      if ($LASTEXITCODE -ne 0 -or ([string]$preservedBranch).Trim() -ne $oldHead) {
        $rollbackError = [InvalidOperationException]::new("Git symbolic branch changed during rollback")
      } else {
        & git symbolic-ref HEAD $oldSym 2>$null
        if ($LASTEXITCODE -ne 0) { $rollbackError = [InvalidOperationException]::new("Git symbolic HEAD rollback failed") }
      }
    }
  }
  if ($indexChanged) {
    try {
      if ((Get-Content -Raw -LiteralPath $lock) -eq $marker -and (Test-Path -LiteralPath $replaceBackup -PathType Leaf) -and [Linq.Enumerable]::SequenceEqual([IO.File]::ReadAllBytes($index), [IO.File]::ReadAllBytes($verify))) {
        [IO.File]::Replace($replaceBackup, $index, $rollbackBackup)
      }
    } catch {
      if ($null -eq $rollbackError) { $rollbackError = $_ }
    }
  }
  if ($rollbackError) { throw $rollbackError }
  throw $failure
} finally {
  Remove-Item Env:GIT_INDEX_FILE -ErrorAction SilentlyContinue; & git update-ref -d $tmpRef 2>$null; $null = $LASTEXITCODE
  Remove-Item -LiteralPath $compare,$candidate,$verify,$replaceBackup,$rollbackBackup -Force -ErrorAction SilentlyContinue; if ((Test-Path -LiteralPath $lock) -and (Get-Content -Raw -LiteralPath $lock) -eq $marker) { Remove-Item -LiteralPath $lock -Force }
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
		if !validEnvName(key) {
			continue
		}
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
	b.WriteString(`$ErrorActionPreference = "Stop"` + "\n")
	b.WriteString(`Set-Location -LiteralPath ` + psQuote(workdir) + "\n")
	b.WriteString(`Add-Type -Name N -Namespace Cbx -MemberDefinition '[DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)]public static extern uint GetFinalPathNameByHandle(Microsoft.Win32.SafeHandles.SafeFileHandle h,System.Text.StringBuilder p,uint n,uint f);[DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)]public static extern Microsoft.Win32.SafeHandles.SafeFileHandle CreateFile(string p,uint a,System.IO.FileShare s,System.IntPtr z,System.IO.FileMode m,uint f,System.IntPtr t);'
function FinalPath($h){$b=New-Object Text.StringBuilder 32768;if(-not[Cbx.N]::GetFinalPathNameByHandle($h,$b,$b.Capacity,0)){throw 'final path failed'};$p=$b.ToString();if($p.StartsWith('\\?\UNC\')){return '\\'+$p.Substring(8)};if($p.StartsWith('\\?\')){return $p.Substring(4)};return $p}
$rh=[Cbx.N]::CreateFile($pwd.Path,0,([IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete),[IntPtr]::Zero,[IO.FileMode]::Open,0x02000000,[IntPtr]::Zero);if($rh.IsInvalid){throw 'workdir open failed'};try{$r=FinalPath $rh}finally{$rh.Dispose()};$q=$r.TrimEnd('\')+'\'
@(`)
	for i, path := range paths {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(psQuote(path))
	}
	b.WriteString(`)|ForEach-Object{$f=$_;$s=$null;try{if([IO.Path]::IsPathRooted($f)){$o=$f}else{$o=Join-Path $pwd.Path $f};$s=[IO.File]::Open($o,[IO.FileMode]::Open,[IO.FileAccess]::Read,([IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete));$p=FinalPath $s.SafeFileHandle;if($p.StartsWith($q,[StringComparison]::Ordinal)){$z=New-Object IO.StreamReader($s,[Text.Encoding]::UTF8,$true,4096,$true);try{$v=$z.ReadToEnd()}finally{$z.Dispose()};Write-Output "` + resultFileMarker + `${f}";Write-Output $v}}catch{}finally{if($s){$s.Dispose()}}}`)
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
