package cli

import "fmt"

const windowsSFTPPrelude = `$ErrorActionPreference='Stop'
function Get-CopyAttributes([string]$Path, [bool]$Optional=$false) {
  try { return [IO.File]::GetAttributes($Path) }
  catch [IO.FileNotFoundException] { if ($Optional) { return $null }; throw }
  catch [IO.DirectoryNotFoundException] { if ($Optional) { return $null }; throw }
}
function Assert-CopyDirectory([string]$Path) {
  $current=$Path
  while ($current) {
    $attributes=Get-CopyAttributes $current
    if (($attributes -band [IO.FileAttributes]::ReparsePoint) -or -not ($attributes -band [IO.FileAttributes]::Directory)) {
      throw 'copy parent is not a regular directory'
    }
    $current=[IO.Path]::GetDirectoryName($current)
  }
}
function Assert-CopyFile([string]$Path, [bool]$Optional=$false) {
  Assert-CopyDirectory ([IO.Path]::GetDirectoryName($Path))
  $attributes=Get-CopyAttributes $Path $Optional
  if ($null -eq $attributes) { return $false }
  if (($attributes -band [IO.FileAttributes]::ReparsePoint) -or ($attributes -band [IO.FileAttributes]::Directory) -or ($attributes -band [IO.FileAttributes]::Device)) {
    throw 'copy supports regular files only'
  }
  return $true
}
function Get-CopyMetadata([string]$Path) {
  Assert-CopyFile $Path | Out-Null
  $file=[IO.File]::Open($Path,[IO.FileMode]::Open,[IO.FileAccess]::Read,[IO.FileShare]::Read)
  try {
    $hash=[Security.Cryptography.SHA256]::Create()
    try { return @{size=[int64]$file.Length;sha256=([BitConverter]::ToString($hash.ComputeHash($file))).Replace('-','')} }
    finally { $hash.Dispose() }
  } finally { $file.Dispose() }
}
function Assert-CopyOwner([string]$Stage,[string]$Owner) {
  Assert-CopyDirectory $Stage
  $marker=Join-Path $Stage 'owner'
  Assert-CopyFile $marker | Out-Null
  if ([IO.File]::ReadAllText($marker) -cne $Owner) { throw 'copy staging ownership changed' }
}
`

func windowsSFTPMetadataScript(source string) string {
	return windowsSFTPPrelude + "\n$metadata=Get-CopyMetadata " + psQuote(source) +
		"\n[Console]::Out.WriteLine('{{\"size\":{0},\"sha256\":\"{1}\"}}', $metadata.size.ToString([Globalization.CultureInfo]::InvariantCulture), $metadata.sha256)\n"
}

func windowsSFTPPrepareScript(destination, stage, owner string) string {
	return windowsSFTPPrelude + fmt.Sprintf(`
Assert-CopyFile %s $true | Out-Null
$stage=%s
if ($null -ne (Get-CopyAttributes $stage $true)) { throw 'copy staging path already exists' }
New-Item -ItemType Directory -Path $stage -ErrorAction Stop | Out-Null
$marker=[IO.File]::Open((Join-Path $stage 'owner'),[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
try {
  $bytes=[Text.Encoding]::UTF8.GetBytes(%s)
  $marker.Write($bytes,0,$bytes.Length)
  $marker.Flush($true)
} finally { $marker.Dispose() }
`, psQuote(destination), psQuote(stage), psQuote(owner))
}

func windowsSFTPPublishScript(destination, stage, owner string, expected windowsSFTPMetadata) string {
	return windowsSFTPPrelude + fmt.Sprintf(`
$stage=%s
Assert-CopyOwner $stage %s
$payload=Join-Path $stage 'payload'
$metadata=Get-CopyMetadata $payload
if ($metadata.size -ne [int64]%d -or $metadata.sha256 -ine %s) { throw 'uploaded file failed size/SHA-256 verification' }
$file=[IO.File]::Open($payload,[IO.FileMode]::Open,[IO.FileAccess]::ReadWrite,[IO.FileShare]::Read)
try { $file.Flush($true) } finally { $file.Dispose() }
$destination=%s
if (Assert-CopyFile $destination $true) {
  [IO.File]::Replace($payload,$destination,[NullString]::Value)
} else {
  [IO.File]::Move($payload,$destination)
}
`, psQuote(stage), psQuote(owner), expected.Size, psQuote(expected.SHA256), psQuote(destination))
}

func windowsSFTPCleanupScript(stage, owner string) string {
	return windowsSFTPPrelude + fmt.Sprintf(`
$stage=%s
if ($null -eq (Get-CopyAttributes $stage $true)) { return }
Assert-CopyOwner $stage %s
foreach ($entry in [IO.Directory]::EnumerateFileSystemEntries($stage)) {
  if ([IO.Path]::GetFileName($entry) -notin @('owner','payload')) { throw 'unrecognized copy staging contents; retained for recovery' }
}
$payload=Join-Path $stage 'payload'
if (Assert-CopyFile $payload $true) { [IO.File]::Delete($payload) }
$marker=Join-Path $stage 'owner'
[IO.File]::Delete($marker)
try { [IO.Directory]::Delete($stage,$false) }
catch {
  $original=$_
  $file=[IO.File]::Open($marker,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
  try {
    $bytes=[Text.Encoding]::UTF8.GetBytes(%s)
    $file.Write($bytes,0,$bytes.Length)
    $file.Flush($true)
  } finally { $file.Dispose() }
  throw $original
}
`, psQuote(stage), psQuote(owner), psQuote(owner))
}
