# Run in a disposable powershell.exe -File process, never dot-source this fixture.
# The watchdog exits this process if a synchronous reader blocks on the async pipe.
param([Parameter(Mandatory=$true)][string]$ReaderPath, [switch]$Block)
$ErrorActionPreference = 'Stop'
Add-Type -TypeDefinition @'
using System;
using System.IO;
using System.IO.Pipes;
using System.Runtime.InteropServices;
using System.Threading;
using System.Threading.Tasks;
using Microsoft.Win32.SafeHandles;

public sealed class CrabboxInputFixture : IDisposable {
    [DllImport("kernel32.dll")] public static extern IntPtr GetStdHandle(int n);
    [DllImport("kernel32.dll")] public static extern bool SetStdHandle(int n, IntPtr handle);
    [DllImport("kernel32.dll")] public static extern uint GetFileType(IntPtr handle);
    [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)]
    static extern SafeFileHandle CreateNamedPipe(string name, uint access, uint mode,
        uint instances, uint outputSize, uint inputSize, uint timeout, IntPtr security);
    [DllImport("kernel32.dll", SetLastError=true)]
    static extern bool ConnectNamedPipe(SafeFileHandle pipe, ref Overlapped overlapped);
    [DllImport("ntdll.dll")]
    static extern int NtQueryInformationFile(SafeFileHandle pipe, out IOStatus status, out uint mode, uint length, int infoClass);
    [StructLayout(LayoutKind.Sequential)]
    struct Overlapped { public IntPtr Internal, InternalHigh; public uint Offset, OffsetHigh; public IntPtr Event; }
    [StructLayout(LayoutKind.Sequential)]
    struct IOStatus { public IntPtr Status; public UIntPtr Information; }
    public readonly SafeFileHandle Input;
    readonly NamedPipeClientStream output;
    readonly IntPtr original;
    readonly CancellationTokenSource cancel = new CancellationTokenSource();
    readonly SemaphoreSlim consumed = new SemaphoreSlim(0);
    readonly Timer watchdog;
    Task writer;
    public static byte[] Payload(int size) {
        byte[] data = new byte[size];
        for (int i = 0; i < size; i++) data[i] = (byte)(i % 251);
        return data;
    }
    public static bool Equal(byte[] a, byte[] b) {
        if (a.Length != b.Length) return false;
        for (int i = 0; i < a.Length; i++) if (a[i] != b[i]) return false;
        return true;
    }
    public CrabboxInputFixture() {
        string name = "crabbox-stdin-test-" + Guid.NewGuid().ToString("N");
        // Keep the read handle unbound to any CLR completion port, just like an
        // inherited Win32 OpenSSH handle. FileStream will bind it for async I/O.
        Input = CreateNamedPipe(@"\\.\pipe\" + name, 0x40000001, 0, 1, 4096, 4096, 0, IntPtr.Zero);
        if (Input.IsInvalid) throw new IOException("cannot create async input pipe");
        IOStatus ioStatus;
        uint mode;
        if (NtQueryInformationFile(Input, out ioStatus, out mode, 4, 16) != 0 || (mode & 0x30) != 0)
            throw new IOException("fixture stdin is not an overlapped handle");
        output = new NamedPipeClientStream(".", name, PipeDirection.Out, PipeOptions.Asynchronous);
        output.Connect(5000);
        // The client is already connected: this completes synchronously with
        // ERROR_PIPE_CONNECTED, without issuing pending I/O or binding the handle.
        Overlapped connection = new Overlapped();
        if (!ConnectNamedPipe(Input, ref connection) && Marshal.GetLastWin32Error() != 535)
            throw new IOException("cannot accept connected input pipe");
        original = GetStdHandle(-10);
        if (!SetStdHandle(-10, Input.DangerousGetHandle()))
            throw new IOException("cannot install fixture stdin");
        watchdog = new Timer(delegate { Environment.Exit(124); }, null, 20000, Timeout.Infinite);
    }
    public void Consumed() { consumed.Release(); }
    public void Send(byte[] data, int chunk, bool waitForRead) {
        // No bytes exist in the pipe until the receiver's READY notification.
        writer = Task.Run(async delegate {
            try {
                for (int i = 0; i < data.Length; i += chunk) {
                    await output.WriteAsync(data, i, Math.Min(chunk, data.Length - i), cancel.Token);
                    if (waitForRead) await consumed.WaitAsync(cancel.Token);
                }
            } finally { output.Dispose(); } // Immediate EOF, including empty input.
        });
    }
    public void FinishWriter() { if (writer != null) writer.GetAwaiter().GetResult(); }
    public void Dispose() {
        SetStdHandle(-10, original);
        Input.Dispose();
        cancel.Cancel();
        output.Dispose();
        if (writer != null) {
            try { if (!writer.Wait(5000)) throw new IOException("fixture writer did not stop"); }
            catch (AggregateException) { } // Broken pipe after an intentionally failed reader.
        }
        watchdog.Dispose();
        cancel.Dispose();
        consumed.Dispose();
    }
}
public sealed class CrabboxInputDestination : MemoryStream {
    readonly CrabboxInputFixture pipe;
    public int Writes;
    public CrabboxInputDestination(CrabboxInputFixture pipe) { this.pipe = pipe; }
    public override void Write(byte[] bytes, int offset, int count) {
        base.Write(bytes, offset, count);
        Writes++;
        pipe.Consumed();
    }
}
public sealed class CrabboxFailingDestination : MemoryStream {
    public override void Write(byte[] bytes, int offset, int count) {
        throw new IOException("fixture destination write failed");
    }
}
'@

if ($Block) {
    $pipe = [CrabboxInputFixture]::new()
    try {
        $destination = [IO.MemoryStream]::new()
        [Console]::Out.WriteLine('READY')
        [Console]::Out.Flush()
        & $ReaderPath -Expected 4991
        throw 'empty open pipe unexpectedly completed'
    } finally { $destination.Dispose(); $pipe.Dispose() }
}

$cases = @(
    @{ Name='empty'; Size=0; Expected=0; Chunk=1 },
    @{ Name='zero-before-input'; Size=4991; Expected=4991; Chunk=4991 },
    @{ Name='partial'; Size=4991; Expected=4991; Chunk=7 },
    @{ Name='4k-boundary'; Size=4991; Expected=4991; Chunk=4991 },
    @{ Name='8k-boundary'; Size=8193; Expected=8193; Chunk=8193 },
    @{ Name='64k-boundary'; Size=65537; Expected=65537; Chunk=65537 },
    @{ Name='large-binary'; Size=2097169; Expected=2097169; Chunk=65537 },
    @{ Name='short'; Size=4991; Expected=4992; Chunk=997; Failure='SSH stdin ended before the framed payload' },
    @{ Name='write-error'; Size=4991; Expected=4991; Chunk=4991; Failure='fixture destination write failed' }
)
$zeroReaderPath = Join-Path (Split-Path -Parent $ReaderPath) 'reader-zero.ps1'
foreach ($case in $cases) {
    $pipe = [CrabboxInputFixture]::new()
    $destination = [CrabboxInputDestination]::new($pipe)
    if ($case.Name -eq 'write-error') { $destination.Dispose(); $destination = [CrabboxFailingDestination]::new() }
    try {
        $bytes = [CrabboxInputFixture]::Payload($case.Size)
        [Console]::Out.WriteLine(('READY ' + $case.Name))
        [Console]::Out.Flush()
        $pipe.Send($bytes, $case.Chunk, ($case.Name -eq 'partial' -or $case.Name -eq 'zero-before-input'))
        # Empty input is already at EOF before either zero-frame reader runs.
        if ($case.Size -eq 0) { $pipe.FinishWriter() }
        if ($case.Name -eq 'zero-before-input') {
            & $ReaderPath -Expected 0
            & $zeroReaderPath -Expected 0
            if ($destination.Length -ne 0) { throw 'zero frame consumed input' }
        }
        $failure = $null
        try { & $ReaderPath -Expected $case.Expected } catch { $failure = $_.Exception.Message }
        if ([CrabboxInputFixture]::GetFileType([CrabboxInputFixture]::GetStdHandle(-10)) -ne 3) {
            throw 'reader closed process-owned stdin'
        }
        if ($case.Failure) {
            if (-not $failure -or -not $failure.Contains($case.Failure)) { throw "wrong failure for $($case.Name): $failure" }
        } else {
            if ($failure) { throw $failure }
            & $zeroReaderPath -Expected 0
            if ($case.Size -eq 0 -and ('Cbx.SshStdin' -as [type])) {
                throw 'zero frame initialized the native stdin helper'
            }
            $pipe.FinishWriter()
            $actual = $destination.ToArray()
            if (-not [CrabboxInputFixture]::Equal($actual, $bytes)) { throw 'payload bytes changed' }
            if ($case.Name -eq 'partial' -and $destination.Writes -lt 2) { throw 'partial reads were not exercised' }
            # Re-enter in this PowerShell process: helper type is already defined,
            # and disposing the first wrapper must leave the borrowed handle valid.
            & $ReaderPath -Expected 0
        }
        [Console]::Out.WriteLine(('PASS ' + $case.Name))
    } finally { $destination.Dispose(); $pipe.Dispose() }
}
[Console]::Out.WriteLine('NATIVE_STDIN_CONTRACT_COMPLETE')
