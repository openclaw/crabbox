$ErrorActionPreference = 'Stop'
$n = '@NONCE@'
$p = Join-Path $HOME ('.crabbox\wsl-stage\@NAME@')
Add-Type @'
using System;
using System.Runtime.InteropServices;
using Microsoft.Win32.SafeHandles;
public static class CBStage {
    [DllImport("kernel32", CharSet=CharSet.Unicode, SetLastError=true)]
    public static extern SafeFileHandle CreateFileW(string p, uint a, uint s, IntPtr x, uint c, uint f, IntPtr t);
    [DllImport("kernel32", SetLastError=true)]
    public static extern bool GetFileInformationByHandleEx(SafeFileHandle h, int c, out long a, uint n);
    [DllImport("kernel32", SetLastError=true)]
    public static extern bool SetFileInformationByHandle(SafeFileHandle h, int c, ref byte v, uint n);
}
'@
$h = [CBStage]::CreateFileW($p, 2147549184, 0, [IntPtr]::Zero, 3, 136314880, [IntPtr]::Zero)
if ($h.IsInvalid) {
    $e = [Runtime.InteropServices.Marshal]::GetLastWin32Error()
    if (@DISCARD@ -and $e -in 2,3) { exit 0 }
    throw 'WSL2 stage open failed'
}
$f = $null
try {
    $a = 0L
    if (![CBStage]::GetFileInformationByHandleEx($h, 9, [ref]$a, 8) -or ($a -band 1040)) { throw 'invalid WSL2 stage file' }
    $f = [IO.FileStream]::new($h, [IO.FileAccess]::Read)
    if ($f.Length -ne @SIZE@L) { throw 'WSL2 stage length mismatch' }
    $sha = [Security.Cryptography.SHA256]::Create()
    try { $digest = [Convert]::ToBase64String($sha.ComputeHash($f)) } finally { $sha.Dispose() }
    if ($digest -cne '@DIGEST@') { throw 'WSL2 stage digest mismatch' }
    $delete = [byte]1
    if (![CBStage]::SetFileInformationByHandle($h, 4, [ref]$delete, 1)) { throw 'WSL2 stage disposition failed' }
    if (@DISCARD@) { exit 0 }
    $f.Position = 0
    $r = [IO.BinaryReader]::new($f)
    $d = $r.ReadBytes(80)
    if ($d.Length -ne 80 -or [Text.Encoding]::ASCII.GetString($d,0,8) -cne 'CBXFLAT2') { throw 'invalid WSL2 descriptor' }
    $w = [BitConverter]::ToUInt32($d,8)
    if (!$w -or $w -gt 32768) { throw 'invalid WSL2 program length' }
    $b = $r.ReadBytes($w)
    if ($b.Length -ne $w) { throw 'incomplete WSL2 program' }
    $utf8 = [Text.UTF8Encoding]::new($false,$true)
    & ([ScriptBlock]::Create($utf8.GetString($b))) $f $d $n
} finally {
    if ($f) { $f.Dispose() } else { $h.Dispose() }
}
