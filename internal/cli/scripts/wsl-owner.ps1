param($file, [byte[]]$descriptor, [string]$nonce)
$ErrorActionPreference = 'Stop'
$helperSize = [BitConverter]::ToUInt32($descriptor,12)
$commandSize = [BitConverter]::ToUInt64($descriptor,16)
$inputSize = [BitConverter]::ToUInt64($descriptor,24)
$timeout = [BitConverter]::ToUInt64($descriptor,32)
$idle = [BitConverter]::ToUInt32($descriptor,40)
$grace = [BitConverter]::ToUInt32($descriptor,44)
if (!$helperSize -or $helperSize -gt 32768 -or $commandSize -gt 67108864 -or
    $inputSize -gt 1099511627776 -or !$idle -or !$grace -or
    $file.Length - $file.Position -ne $helperSize + $commandSize + $inputSize) {
    throw 'invalid WSL2 envelope lengths'
}
$helper = [IO.BinaryReader]::new($file).ReadBytes($helperSize)
if ($helper.Length -ne $helperSize -or $helper -contains 0) { throw 'invalid WSL2 helper' }
$utf8 = [Text.UTF8Encoding]::new($false,$true)
$null = $utf8.GetString($helper)
$bootstrap = @BOOTSTRAP@
$directory = '/tmp/crabbox-command-' + $nonce
$clock = [Diagnostics.Stopwatch]::StartNew()
$phase = 'launcher-start'
$read = 0L
$written = 0L
$process = $null
$writer = $null
$failure = $null

function Remaining([long]$ceiling) {
    if ($timeout) {
        $left = [long]$timeout - $clock.ElapsedMilliseconds
        if ($left -le 0) { throw 'WSL2 command timed out' }
        return [int][Math]::Min($ceiling,$left)
    }
    return [int][Math]::Min($ceiling,[int]::MaxValue)
}
function Wait-Pipe($task, [switch]$startup) {
    $ceiling = $idle
    if ($startup) {
        # Opening and helper delivery share one bounded startup clock.
        $ceiling = @STARTUP@ - $clock.ElapsedMilliseconds
        if ($ceiling -le 0) { throw 'WSL2 command timed out' }
    }
    if (!$task.Wait((Remaining $ceiling))) {
        $null = Remaining $ceiling
        throw 'WSL2 pipe transfer made no progress'
    }
    $null = $task.GetAwaiter().GetResult()
}
function Write-Pipe($stream, [byte[]]$bytes, [int]$count, [switch]$startup) {
    Wait-Pipe ($stream.WriteAsync($bytes,0,$count)) -startup:$startup
}
function Start-Linux([string]$mode) {
    $info = [Diagnostics.ProcessStartInfo]::new('wsl.exe')
    $info.UseShellExecute = $false
    $info.RedirectStandardInput = $true
    # Framework 5.1 may emit Console.InputEncoding's preamble during Start.
    $encoding = [Console]::InputEncoding
    if ($PSVersionTable.PSEdition -eq 'Core') {
        $info.StandardInputEncoding = $utf8
        $encoding = $utf8
    }
    $preamble = $encoding.GetPreamble()
    if ([BitConverter]::ToString($preamble) -notin @('','EF-BB-BF')) { throw 'unsupported WSL2 pipe preamble' }
    $info.Arguments = '--exec sh -c "' + $bootstrap.Replace('"','\"') + '" sh ' +
        $helperSize + ' ' + $preamble.Length + ' ' + $mode + ' ' + $directory + ' ' + $nonce + ' ' +
        $commandSize + ' ' + $inputSize + ' ' + $idle + ' ' + $grace
    return [Diagnostics.Process]::Start($info)
}
function Open-LinuxInput($child) {
    Wait-Pipe ($child.StandardInput.FlushAsync()) -startup
    # One unbuffered view keeps helper and frame bytes on the same handle.
    return [IO.FileStream]::new($child.StandardInput.BaseStream.SafeFileHandle,[IO.FileAccess]::Write,1,$false)
}
function Stop-Exact($child) {
    try { if ($child.HasExited) { return $true }; $child.Kill() } catch {}
    try { return $child.WaitForExit(5000) } catch { return $false }
}
try {
    $process = Start-Linux 'run'
    $phase = 'pipe-open'
    $writer = Open-LinuxInput $process
    $phase = 'helper-write'
    Write-Pipe $writer $helper $helper.Length -startup
    $buffer = [byte[]]::new(65536)
    $remaining = [long]$commandSize + [long]$inputSize
    while ($remaining -gt 0) {
        $phase = 'file-read'
        $count = $file.Read($buffer,0,[int][Math]::Min($buffer.Length,$remaining))
        if (!$count) { throw 'WSL2 envelope ended early' }
        $read += $count
        $phase = 'pipe-write'
        Write-Pipe $writer $buffer $count
        $written += $count
        $remaining -= $count
    }
    # Complete frame delivery is not control EOF; the watcher owns that signal.
    $phase = 'execute'
    if ($timeout) {
        if (!$process.WaitForExit((Remaining ([int]::MaxValue)))) { throw 'WSL2 command timed out' }
    } else { $process.WaitForExit() }
    $code = $process.ExitCode
} catch {
    $reason = $_.Exception.Message
    if ($reason -notin @('WSL2 command timed out','WSL2 pipe transfer made no progress','WSL2 envelope ended early')) {
        $reason = 'WSL2 transport failed'
    }
    $failure = $reason + ' phase=' + $phase + ' expected=' + ($commandSize + $inputSize) + ' read=' + $read + ' written=' + $written
} finally {
    if ($writer) { $writer.Dispose() }
}
if ($failure) {
    if ($process -and !$process.WaitForExit(5000) -and !(Stop-Exact $process)) {
        [Console]::Error.WriteLine('WSL2 command cleanup failed: launcher termination unconfirmed')
        throw $failure
    }
    # Cleanup starts only after the original exact launcher is reaped.
    $cleanup = $null
    $cleanupWriter = $null
    try {
        $timeout = 10000
        $clock.Restart()
        $cleanup = Start-Linux 'cleanup'
        $cleanupWriter = Open-LinuxInput $cleanup
        Write-Pipe $cleanupWriter $helper $helper.Length -startup
        $cleanupWriter.Dispose()
        if (!$cleanup.WaitForExit((Remaining 10000))) { throw 'cleanup timeout' }
        if ($cleanup.ExitCode -ne 0) { throw 'cleanup refused' }
    } catch {
        $confirmed = !$cleanup -or (Stop-Exact $cleanup)
        if ($confirmed) { [Console]::Error.WriteLine('WSL2 command cleanup failed') }
        else { [Console]::Error.WriteLine('WSL2 command cleanup failed: cleanup launcher termination unconfirmed') }
    } finally {
        if ($cleanupWriter) { $cleanupWriter.Dispose() }
        if ($cleanup) { $cleanup.Dispose() }
    }
    throw $failure
}
$process.Dispose()
exit $code
