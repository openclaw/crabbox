$ErrorActionPreference = 'Stop'
$path = Join-Path $HOME ('.crabbox\wsl-stage\@NAME@')
$discard = @DISCARD@
Add-Type @'
using System;
using System.Runtime.InteropServices;
using Microsoft.Win32.SafeHandles;
public static class CBXStage {
    [DllImport("kernel32", CharSet=CharSet.Unicode, SetLastError=true)]
    public static extern SafeFileHandle CreateFileW(string p, uint a, uint s, IntPtr x, uint c, uint f, IntPtr t);
    [DllImport("kernel32", SetLastError=true)]
    public static extern bool GetFileInformationByHandleEx(SafeFileHandle h, int c, out long a, uint n);
    [DllImport("kernel32", SetLastError=true)]
    public static extern bool SetFileInformationByHandle(SafeFileHandle h, int c, ref byte v, uint n);
}
'@
$handle = [CBXStage]::CreateFileW($path, 2147549184, 0, [IntPtr]::Zero, 3, 136314880, [IntPtr]::Zero)
if ($handle.IsInvalid) {
    $errorCode = [Runtime.InteropServices.Marshal]::GetLastWin32Error()
    if ($discard -and $errorCode -in 2,3) { exit 0 }
    throw 'WSL2 stage open failed'
}
$stream = $null
try {
    $attributes = 0L
    if (![CBXStage]::GetFileInformationByHandleEx($handle, 9, [ref]$attributes, 8) -or ($attributes -band 1040)) {
        throw 'invalid WSL2 stage file'
    }
    $stream = [IO.FileStream]::new($handle, [IO.FileAccess]::Read)
    if ($stream.Length -ne @SIZE@L) { throw 'WSL2 stage length mismatch' }
    $sha = [Security.Cryptography.SHA256]::Create()
    try { $actual = [Convert]::ToBase64String($sha.ComputeHash($stream)) } finally { $sha.Dispose() }
    if ($actual -cne '@DIGEST@') { throw 'WSL2 stage digest mismatch' }
    $delete = [byte]1
    if (![CBXStage]::SetFileInformationByHandle($handle, 4, [ref]$delete, 1)) {
        throw 'WSL2 stage disposition failed'
    }
    if (!$discard) {
        $stream.Position = 0
        $reader = [IO.BinaryReader]::new($stream)
        $descriptor = $reader.ReadBytes(80)
        if ($descriptor.Length -ne 80 -or [Text.Encoding]::ASCII.GetString($descriptor,0,8) -cne 'CBXWSL3!') {
            throw 'invalid WSL2 descriptor'
        }
        $ownerSize = [BitConverter]::ToUInt32($descriptor,8)
        if (!$ownerSize -or $ownerSize -gt 32768) { throw 'invalid WSL2 owner length' }
        $owner = $reader.ReadBytes($ownerSize)
        if ($owner.Length -ne $ownerSize) { throw 'incomplete WSL2 owner' }
        $utf8 = [Text.UTF8Encoding]::new($false,$true)
        & ([ScriptBlock]::Create($utf8.GetString($owner))) $stream $descriptor '@NONCE@'
        $code = $LASTEXITCODE
    }
} finally {
    if ($stream) { $stream.Dispose() } else { $handle.Dispose() }
}
if ($discard) {
    if (Test-Path -LiteralPath $path) { throw 'WSL2 stage deletion unconfirmed' }
    exit 0
}
exit $code
