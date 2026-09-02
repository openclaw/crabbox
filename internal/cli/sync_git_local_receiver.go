package cli

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

const localGitSeedHeadRef = "refs/crabbox/local-head"

// gitLocalSeedPlan binds receiver metadata to one self-contained local artifact.
// The ordinary sync manifest remains the sole authority for working files.
type gitLocalSeedPlan struct {
	Head, Tree, ObjectFormat, Digest string
	Refs                             []localGitSeedRef
	PackedBytes                      int64
	Fingerprint                      string
}

func (p gitLocalSeedPlan) valid() bool {
	n := 40
	if p.ObjectFormat == "sha256" {
		n = 64
	} else if p.ObjectFormat != "sha1" {
		return false
	}
	hex := func(s string, size int) bool {
		return len(s) == size && strings.Trim(s, "0123456789abcdef") == ""
	}
	if !hex(p.Head, n) || !hex(p.Tree, n) || !hex(p.Digest, 64) || p.PackedBytes <= 0 || (p.Fingerprint != "" && !hex(p.Fingerprint, 64)) || len(p.Refs) == 0 {
		return false
	}
	seen := make(map[string]bool, len(p.Refs))
	headRef := false
	for _, ref := range p.Refs {
		if !hex(ref.OID, n) || !strings.HasPrefix(ref.Name, "refs/") || strings.ContainsAny(ref.Name, "\x00\r\n\t ") || seen[ref.Name] {
			return false
		}
		seen[ref.Name] = true
		if ref.Name == localGitSeedHeadRef {
			headRef = ref.OID == p.Head
		}
	}
	return headRef
}

func (p gitLocalSeedPlan) refLines() string {
	lines := make([]string, len(p.Refs))
	for i, ref := range p.Refs {
		lines[i] = ref.OID + " " + ref.Name
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func (p gitLocalSeedPlan) refsDigest() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(p.refLines())))
}

func (p gitLocalSeedPlan) metadataRefsDigest() string {
	refs := p.Refs
	p.Refs = nil
	for _, ref := range refs {
		// The bundle's HEAD anchor is transport metadata, not a receiver ref.
		if ref.Name != localGitSeedHeadRef {
			p.Refs = append(p.Refs, ref)
		}
	}
	return p.refsDigest()
}

func remoteGitLocalSeed(workdir string, plan gitLocalSeedPlan) string {
	return remoteGitLocalSeedCommand(workdir, plan, "import")
}

func remoteGitLocalSeedFinalize(workdir string, plan gitLocalSeedPlan) string {
	return remoteGitLocalSeedCommand(workdir, plan, "finalize")
}

func remoteGitLocalSeedFingerprint(workdir string, plan gitLocalSeedPlan) string {
	return remoteGitLocalSeedCommand(workdir, plan, "fingerprint")
}

func remoteGitLocalSeedCommand(workdir string, plan gitLocalSeedPlan, action string) string {
	if !plan.valid() {
		return remoteHermeticPOSIXControlCommand("echo 'local Git seed: invalid receiver plan' >&2; exit 67")
	}
	script := `set -eu
umask 077
	` + strings.Replace(remotePlainManifestGitFunction(), "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1 GIT_NO_REPLACE_OBJECTS=1 GIT_OPTIONAL_LOCKS=0", 1) + `
workdir=` + shellPathQuote(workdir) + `
expected_head=` + shellQuote(plan.Head) + `
expected_tree=` + shellQuote(plan.Tree) + `
expected_format=` + shellQuote(plan.ObjectFormat) + `
expected_digest=` + shellQuote(plan.Digest) + `
expected_refs_digest=` + shellQuote(plan.refsDigest()) + `
expected_metadata_refs_digest=` + shellQuote(plan.metadataRefsDigest()) + `
transport_head_ref=` + shellQuote(localGitSeedHeadRef) + `
expected_fingerprint=` + shellQuote(plan.Fingerprint) + `
stage=
backup=
finalizing=
phase=prepare
fail() { echo "local Git seed: $phase failed" >&2; exit 67; }
ownership_fail() {
  echo "local Git seed: ownership failed; existing Git metadata is not owned by local seeding. Use --full-resync only if you intend to replace this workspace." >&2
  exit 67
}
hash_stdin() {
  if command -v sha256sum >/dev/null 2>&1; then
    digest="$(sha256sum)" || return 1
  else
    digest="$(shasum -a 256)" || return 1
  fi
  printf '%s' "${digest%% *}"
}
cleanup() {
  result=$?
  trap - EXIT
  if [ "$result" -ne 0 ] && [ -n "$finalizing" ]; then
    rm -f -- "$metadata/crabbox-local-complete" "$metadata/crabbox/sync-fingerprint" || result=67
  fi
  if [ -z "$backup" ] && [ -n "$stage" ] && [ -d "$stage" ] && [ ! -L "$stage" ]; then
    if ! rm -rf -- "$stage"; then
      echo "local Git seed: retained metadata at $stage" >&2
      result=67
    fi
  elif [ -z "$backup" ] && [ -n "$stage" ] && { [ -e "$stage" ] || [ -L "$stage" ]; }; then
    echo "local Git seed: retained metadata at $stage" >&2
    result=67
  fi
  if [ -n "$backup" ]; then
    echo "local Git seed: retained previous metadata at $backup" >&2
    result=67
  fi
  exit "$result"
}

trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -p -- "$workdir" || fail
cd -P -- "$workdir" || fail
workdir="$PWD"
metadata="$workdir/.git"
owned_metadata() {
  [ -d "$metadata" ] && [ ! -L "$metadata" ] || return 1
  [ -f "$metadata/crabbox-local-owner" ] && [ ! -L "$metadata/crabbox-local-owner" ] || return 1
  owner="$(cat "$metadata/crabbox-local-owner")" || return 1
  [ "$owner" = crabbox-local-seed-v1 ] || return 1
  # This directory belongs to the local receiver, never a source repository.
  links="$(find -P "$metadata" -type l -print -quit)" || return 1
  [ -z "$links" ] || return 1
}
verify_metadata() {
  owned_metadata || return 1
  [ "$(cat "$metadata/crabbox-local-artifact" 2>/dev/null)" = "$expected_digest" ] || return 1
  [ "$(plain_git config --file "$metadata/config" --no-includes --get core.bare 2>/dev/null)" = false ] || return 1
  [ "$(plain_git --git-dir="$metadata" rev-parse --show-object-format 2>/dev/null)" = "$expected_format" ] || return 1
  [ "$(plain_git --git-dir="$metadata" rev-parse --verify 'HEAD^{commit}' 2>/dev/null)" = "$expected_head" ] || return 1
  [ "$(cat "$metadata/HEAD")" = "$expected_head" ] || return 1
  [ "$(plain_git --git-dir="$metadata" rev-parse --verify 'HEAD^{tree}' 2>/dev/null)" = "$expected_tree" ] || return 1
  [ "$(plain_git --git-dir="$metadata" write-tree 2>/dev/null)" = "$expected_tree" ] || return 1
  actual_refs="$(plain_git --git-dir="$metadata" for-each-ref --format='%(objectname) %(refname)' 2>/dev/null)" || return 1
  actual_refs="$(printf '%s\n' "$actual_refs" | sort)" || return 1
  [ "$(printf '%s' "$actual_refs" | hash_stdin)" = "$expected_metadata_refs_digest" ] || return 1
  [ ! -e "$metadata/shallow" ] && [ ! -e "$metadata/objects/info/alternates" ] || return 1
  [ ! -e "$metadata/config.worktree" ] && [ ! -e "$metadata/info/grafts" ] || return 1
  config_keys="$(plain_git config --file "$metadata/config" --no-includes --name-only --list 2>/dev/null)" || return 1
  while IFS= read -r key; do
    case "$key" in core.repositoryformatversion|core.filemode|core.bare|core.logallrefupdates|core.ignorecase|core.precomposeunicode|core.symlinks|extensions.objectformat) ;; *) return 1 ;; esac
  done <<EOF
$config_keys
EOF
  plain_git --git-dir="$metadata" fsck --full --strict --no-reflogs >/dev/null 2>&1 || return 1
}
`
	if action == "import" {
		script += fmt.Sprintf(`phase=ownership
if [ -e "$metadata" ] || [ -L "$metadata" ]; then
  owned_metadata || ownership_fail
  rm -f -- "$metadata/crabbox-local-complete" "$metadata/crabbox/sync-fingerprint" || fail
fi
stage="$(mktemp -d "$workdir/.crabbox-local-git.XXXXXXXX")" || fail
bundle="$stage/payload.bundle"
phase=receive
cat > "$bundle" || fail
[ "$(wc -c < "$bundle" | tr -d ' ')" = %d ] || fail
phase=digest
if command -v sha256sum >/dev/null 2>&1; then
  actual_digest="$(sha256sum "$bundle")" || fail
else
  actual_digest="$(shasum -a 256 "$bundle")" || fail
fi
[ "${actual_digest%%%% *}" = "$expected_digest" ] || fail
phase=initialize
fresh="$stage/git"
plain_git init --quiet --template= --object-format="$expected_format" "$stage/repo" >/dev/null 2>&1 || fail
mv -- "$stage/repo/.git" "$fresh" || fail
plain_git --git-dir="$fresh" config core.logAllRefUpdates false >/dev/null 2>&1 || fail
phase=verify-bundle
bundle_refs="$(plain_git --git-dir="$fresh" bundle list-heads "$bundle" 2>/dev/null)" || fail
bundle_refs="$(printf '%%s\n' "$bundle_refs" | sort)" || fail
[ "$(printf '%%s' "$bundle_refs" | hash_stdin)" = "$expected_refs_digest" ] || fail
plain_git --git-dir="$fresh" bundle verify "$bundle" >/dev/null 2>&1 || fail
plain_git --git-dir="$fresh" bundle unbundle "$bundle" >/dev/null 2>&1 || fail
while IFS=' ' read -r oid ref; do
  plain_git check-ref-format "$ref" >/dev/null 2>&1 || fail
  if [ "$ref" = "$transport_head_ref" ]; then continue; fi
  plain_git --git-dir="$fresh" update-ref "$ref" "$oid" >/dev/null 2>&1 || fail
done <<EOF
$bundle_refs
EOF
plain_git --git-dir="$fresh" update-ref --no-deref HEAD "$expected_head" >/dev/null 2>&1 || fail
plain_git --git-dir="$fresh" read-tree "$expected_head" >/dev/null 2>&1 || fail
printf 'crabbox-local-seed-v1' > "$fresh/crabbox-local-owner"
printf '%%s' "$expected_digest" > "$fresh/crabbox-local-artifact"
metadata="$fresh"
phase=verify-metadata
verify_metadata || fail
metadata="$workdir/.git"
phase=publish
# Attaching Git changes the manifest location. Carry prior file ownership into
# the new metadata so ordinary prune still removes files from earlier raw syncs.
manifest_dir="$workdir/.crabbox"
if [ -e "$metadata" ] || [ -L "$metadata" ]; then
  owned_metadata || ownership_fail
  manifest_dir="$metadata/crabbox"
fi
manifest="$manifest_dir/sync-manifest"
if [ -e "$manifest" ] || [ -L "$manifest" ]; then
  [ -d "$manifest_dir" ] && [ ! -L "$manifest_dir" ] || fail
  [ -f "$manifest" ] && [ ! -L "$manifest" ] || fail
  mkdir -- "$fresh/crabbox" || fail
  cp -- "$manifest" "$fresh/crabbox/sync-manifest" || fail
fi
if [ -e "$metadata" ] || [ -L "$metadata" ]; then
  previous="$stage/previous.git"
  mv -- "$metadata" "$previous" || fail
  backup="$previous"
fi
if ! mv -- "$fresh" "$metadata"; then
  if [ -n "$backup" ] && mv -- "$backup" "$metadata"; then backup=; fi
  fail
fi
if [ -n "$backup" ]; then
  if ! rm -rf -- "$backup"; then
    # Keep the stage locator if removal could not complete.
    stage=
    fail
  fi
  backup=
fi
`, plan.PackedBytes)
	} else {
		if action == "finalize" {
			script += `owned_metadata || fail
finalizing=1
rm -f -- "$metadata/crabbox-local-complete" "$metadata/crabbox/sync-fingerprint" || fail
`
		}
		script += `phase=verify-metadata
verify_metadata || fail
`
		if action == "finalize" {
			script += `phase=finalize
mkdir -p -- "$metadata/crabbox" || fail
printf '%s' "$expected_digest" > "$metadata/crabbox-local-complete.new" || fail
mv -- "$metadata/crabbox-local-complete.new" "$metadata/crabbox-local-complete" || fail
printf '%s' "$expected_fingerprint" > "$metadata/crabbox/sync-fingerprint.new" || fail
mv -- "$metadata/crabbox/sync-fingerprint.new" "$metadata/crabbox/sync-fingerprint" || fail
`
		} else {
			script += `[ "$(cat "$metadata/crabbox-local-complete" 2>/dev/null)" = "$expected_digest" ] || fail
[ -n "$expected_fingerprint" ] && [ "$(cat "$metadata/crabbox/sync-fingerprint" 2>/dev/null)" = "$expected_fingerprint" ] || fail
printf '%s' "$expected_fingerprint"
`
		}
	}
	return remoteHermeticPOSIXControlCommand(script)
}

func windowsGitLocalSeed(workdir string, plan gitLocalSeedPlan) string {
	return windowsGitLocalSeedCommand(workdir, plan, false)
}

func windowsGitLocalSeedFinalize(workdir string, plan gitLocalSeedPlan) string {
	return windowsGitLocalSeedCommand(workdir, plan, true)
}

// The mandatory Windows workspace-owner witness supplies ordinary redirected
// stdin here, rather than the raw OpenSSH overlapped input handle.
func windowsGitLocalSeedCommand(workdir string, plan gitLocalSeedPlan, finalize bool) string {
	if !plan.valid() {
		return PowershellCommand("[Console]::Error.WriteLine('local Git seed: invalid receiver plan'); exit 67")
	}
	script := windowsPowerShellPathRefresh + `$ErrorActionPreference = 'Stop'
$workdir = ` + psQuote(workdir) + `
$expectedHead = ` + psQuote(plan.Head) + `
$expectedTree = ` + psQuote(plan.Tree) + `
$expectedFormat = ` + psQuote(plan.ObjectFormat) + `
$expectedDigest = ` + psQuote(plan.Digest) + `
$expectedRefsDigest = ` + psQuote(plan.refsDigest()) + `
$expectedMetadataRefsDigest = ` + psQuote(plan.metadataRefsDigest()) + `
$transportHeadRef = ` + psQuote(localGitSeedHeadRef) + `
$phase = 'prepare'
$stage = $null
$backup = $null
$finalizing = $false
$failed = $false
function Sort-LocalLines($Lines) {
  $values = [Collections.Generic.SortedDictionary[string,string]]::new([StringComparer]::Ordinal)
  foreach ($line in $Lines) {
    $key = [BitConverter]::ToString([Text.Encoding]::UTF8.GetBytes([string]$line))
    $values.Add($key, [string]$line)
  }
  return [string]::Join("` + "`n" + `", [string[]]$values.Values)
}
function Get-LocalRefsDigest([string]$Lines) {
  $hash = [Security.Cryptography.SHA256]::Create()
  try { return [BitConverter]::ToString($hash.ComputeHash([Text.Encoding]::UTF8.GetBytes($Lines))).Replace('-', '').ToLowerInvariant() }
  finally { $hash.Dispose() }
}
function Invoke-LocalGit {
  $ErrorActionPreference = 'Continue'
  $output = & $gitExe -c credential.helper= -c core.hooksPath=NUL -c core.attributesFile=NUL -c core.fsmonitor=false -c protocol.allow=never @args 2>$null
  if ($LASTEXITCODE -ne 0) { throw 'Git operation failed' }
  return $output
}
function Remove-LocalMarker([string]$Path) {
  if (Test-Path -LiteralPath $Path) { Remove-Item -Force -LiteralPath $Path -ErrorAction Stop }
}
function Assert-OwnedMetadata {
  $item = Get-Item -Force -LiteralPath $metadata
  if (-not $item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'metadata is not owned' }
  $owner = Join-Path $metadata 'crabbox-local-owner'
  if ([IO.File]::ReadAllText($owner) -cne 'crabbox-local-seed-v1') { throw 'metadata is not owned' }
  foreach ($child in (Get-ChildItem -Force -Recurse -LiteralPath $metadata)) {
    if ($child.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'metadata layout changed' }
  }
}
function Assert-ImportOwnership {
  try { Assert-OwnedMetadata }
  catch {
    [Console]::Error.WriteLine('local Git seed: ownership failed; existing Git metadata is not owned by local seeding. Use --full-resync only if you intend to replace this workspace.')
    throw
  }
}
function Assert-LocalMetadata {
  Assert-OwnedMetadata
  if ([IO.File]::ReadAllText((Join-Path $metadata 'crabbox-local-artifact')) -cne $expectedDigest) { throw 'artifact mismatch' }
  if ((Invoke-LocalGit config --file (Join-Path $metadata 'config') --no-includes --get core.bare) -cne 'false') { throw 'worktree metadata required' }
  if ((Invoke-LocalGit --git-dir=$metadata rev-parse --show-object-format) -cne $expectedFormat) { throw 'format mismatch' }
  if ((Invoke-LocalGit --git-dir=$metadata rev-parse --verify 'HEAD^{commit}') -cne $expectedHead) { throw 'HEAD mismatch' }
  if ([IO.File]::ReadAllText((Join-Path $metadata 'HEAD')).Trim() -cne $expectedHead) { throw 'HEAD is not detached' }
  if ((Invoke-LocalGit --git-dir=$metadata rev-parse --verify 'HEAD^{tree}') -cne $expectedTree) { throw 'tree mismatch' }
  if ((Invoke-LocalGit --git-dir=$metadata write-tree) -cne $expectedTree) { throw 'index mismatch' }
  $actualRefs = Sort-LocalLines (Invoke-LocalGit --git-dir=$metadata for-each-ref '--format=%(objectname) %(refname)')
  if ((Get-LocalRefsDigest $actualRefs) -cne $expectedMetadataRefsDigest) { throw 'refs mismatch' }
  foreach ($name in @('shallow', 'objects/info/alternates', 'config.worktree', 'info/grafts')) {
    if (Test-Path -LiteralPath (Join-Path $metadata $name)) { throw 'unsupported metadata' }
  }
  foreach ($key in (Invoke-LocalGit config --file (Join-Path $metadata 'config') --no-includes --name-only --list)) {
    if ($key -cnotmatch '^(core\.(repositoryformatversion|filemode|bare|logallrefupdates|ignorecase|precomposeunicode|symlinks)|extensions\.objectformat)$') { throw 'unexpected configuration' }
  }
  $null = Invoke-LocalGit --git-dir=$metadata fsck --full --strict --no-reflogs
}
try {
  $gitExe = (Get-Command git.exe -CommandType Application | Select-Object -First 1).Source
  foreach ($item in @(Get-ChildItem Env:)) {
    if ($item.Name.StartsWith('GIT_', [StringComparison]::OrdinalIgnoreCase)) { [Environment]::SetEnvironmentVariable($item.Name, $null, 'Process') }
  }
  $env:GIT_CONFIG_NOSYSTEM = '1'; $env:GIT_CONFIG_GLOBAL = 'NUL'; $env:GIT_CONFIG_SYSTEM = 'NUL'
  $env:GIT_NO_LAZY_FETCH = '1'; $env:GIT_NO_REPLACE_OBJECTS = '1'; $env:GIT_ATTR_NOSYSTEM = '1'
  $env:GIT_TERMINAL_PROMPT = '0'; $env:GIT_OPTIONAL_LOCKS = '0'; $env:GCM_INTERACTIVE = 'Never'
  $OutputEncoding = [Text.UTF8Encoding]::new($false)
  [Console]::OutputEncoding = $OutputEncoding
  $null = New-Item -ItemType Directory -Force -Path $workdir
  Set-Location -LiteralPath $workdir
  $workdir = (Get-Location).Path
  $metadata = Join-Path $workdir '.git'
`
	if finalize {
		script += `  Assert-OwnedMetadata
  $finalizing = $true
  Remove-LocalMarker (Join-Path $metadata 'crabbox-local-complete')
  $phase = 'verify-metadata'
  Assert-LocalMetadata
  $phase = 'finalize'
  [IO.File]::WriteAllText((Join-Path $metadata 'crabbox-local-complete'), $expectedDigest)
`
	} else {
		script += `  $phase = 'ownership'
  if (Test-Path -LiteralPath $metadata) {
    Assert-ImportOwnership
    Remove-LocalMarker (Join-Path $metadata 'crabbox-local-complete')
    Remove-LocalMarker (Join-Path $metadata 'crabbox/sync-fingerprint')
  }
  $stage = Join-Path $workdir ('.crabbox-local-git.' + [Guid]::NewGuid().ToString('N'))
  $acl = New-Object Security.AccessControl.DirectorySecurity
  $acl.SetAccessRuleProtection($true, $false)
  $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User
  $acl.SetOwner($sid)
  $rule = New-Object Security.AccessControl.FileSystemAccessRule($sid, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')
  $acl.AddAccessRule($rule)
  $directory = New-Object IO.DirectoryInfo($stage)
  $directory.Create($acl)
  $bundle = Join-Path $stage 'payload.bundle'
  $phase = 'receive'
  $payload = [IO.File]::Open($bundle, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
  try {
    $stdin = [Console]::OpenStandardInput()
    try { $stdin.CopyTo($payload) } finally { $stdin.Dispose() }
    if ($payload.Length -ne [Int64]` + fmt.Sprint(plan.PackedBytes) + `) { throw 'payload length mismatch' }
  } finally { $payload.Dispose() }
  $phase = 'digest'
  if ((Get-FileHash -LiteralPath $bundle -Algorithm SHA256).Hash.ToLowerInvariant() -cne $expectedDigest) { throw 'digest mismatch' }
  $phase = 'initialize'
  $repo = Join-Path $stage 'repo'
  $fresh = Join-Path $stage 'git'
  $null = Invoke-LocalGit init --quiet --template= --object-format=$expectedFormat $repo
  [IO.Directory]::Move((Join-Path $repo '.git'), $fresh)
  $null = Invoke-LocalGit --git-dir=$fresh config core.logAllRefUpdates false
  $phase = 'verify-bundle'
  $bundleRefs = Sort-LocalLines (Invoke-LocalGit --git-dir=$fresh bundle list-heads $bundle)
  if ((Get-LocalRefsDigest $bundleRefs) -cne $expectedRefsDigest) { throw 'bundle refs mismatch' }
  $null = Invoke-LocalGit --git-dir=$fresh bundle verify $bundle
  $null = Invoke-LocalGit --git-dir=$fresh bundle unbundle $bundle
  foreach ($line in ($bundleRefs -split "` + "`n" + `")) {
    $parts = $line.Split(' ', 2)
    $null = Invoke-LocalGit check-ref-format $parts[1]
    if ($parts[1] -ceq $transportHeadRef) { continue }
    $null = Invoke-LocalGit --git-dir=$fresh update-ref $parts[1] $parts[0]
  }
  $null = Invoke-LocalGit --git-dir=$fresh update-ref --no-deref HEAD $expectedHead
  $null = Invoke-LocalGit --git-dir=$fresh read-tree $expectedHead
  [IO.File]::WriteAllText((Join-Path $fresh 'crabbox-local-owner'), 'crabbox-local-seed-v1')
  [IO.File]::WriteAllText((Join-Path $fresh 'crabbox-local-artifact'), $expectedDigest)
  $metadata = $fresh
  $phase = 'verify-metadata'
  Assert-LocalMetadata
  $metadata = Join-Path $workdir '.git'
  $phase = 'publish'
  # Match the POSIX handoff: raw ownership is used only on the first attachment.
  $manifestDirectory = Join-Path $workdir '.crabbox'
  if (Test-Path -LiteralPath $metadata) {
    Assert-ImportOwnership
    $manifestDirectory = Join-Path $metadata 'crabbox'
  }
  $manifest = Join-Path $manifestDirectory 'sync-manifest'
  if (Test-Path -LiteralPath $manifest) {
    $directory = Get-Item -Force -LiteralPath $manifestDirectory
    if (-not $directory.PSIsContainer -or ($directory.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'invalid manifest directory' }
    $item = Get-Item -Force -LiteralPath $manifest
    if ($item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'invalid sync manifest' }
    $null = New-Item -ItemType Directory -Path (Join-Path $fresh 'crabbox')
    [IO.File]::Copy($manifest, (Join-Path $fresh 'crabbox/sync-manifest'))
  }
  if (Test-Path -LiteralPath $metadata) {
    $previous = Join-Path $stage 'previous.git'
    [IO.Directory]::Move($metadata, $previous)
    $backup = $previous
  }
  try { [IO.Directory]::Move($fresh, $metadata) }
  catch {
    if ($backup) { [IO.Directory]::Move($backup, $metadata); $backup = $null }
    throw
  }
  if ($backup) { Remove-Item -Recurse -Force -LiteralPath $backup; $backup = $null }
`
	}
	script += `} catch {
  [Console]::Error.WriteLine("local Git seed: $phase failed")
  $failed = $true
} finally {
  if ($failed -and $finalizing) {
    try { Remove-LocalMarker (Join-Path $metadata 'crabbox-local-complete') }
    catch { [Console]::Error.WriteLine("local Git seed: retained metadata at $metadata") }
  }
  if ($backup) {
    [Console]::Error.WriteLine("local Git seed: retained previous metadata at $backup")
    $failed = $true
  } elseif ($stage -and (Test-Path -LiteralPath $stage)) {
    try { Remove-Item -Recurse -Force -LiteralPath $stage }
    catch { [Console]::Error.WriteLine("local Git seed: retained metadata at $stage"); $failed = $true }
  }
}
if ($failed) { exit 67 }
`
	return PowershellCommand(script)
}
