package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Task.Wait and Process.WaitForExit accept signed 32-bit millisecond counts.
// This is a technical wait bound, not a transfer-policy allowance.
const nativeWSLUploadMaxWait = time.Duration(1<<31-1) * time.Millisecond

// nativeWSLUploadCommand renders a finite upload to a trusted POSIX installer.
// SSH stdin must contain exactly installer UTF-8 bytes followed by payloadSize
// binary bytes; the installer receives payloadSize as $1. No payload bytes are
// embedded in the command. The caller supplies a previously established shell
// route and owns artifact validation and the Linux installation lifecycle.
// Timeout cleanup terminates the exact Windows launcher, not the Linux guest.
func nativeWSLUploadCommand(installer string, payloadSize int64, timeout time.Duration, shell wslStageShell) (string, error) {
	if len(installer) == 0 || len(installer) > wslStageMaxHelper || !utf8.ValidString(installer) || strings.ContainsRune(installer, 0) {
		return "", fmt.Errorf("native WSL installer must be nonempty UTF-8 text within %d bytes", wslStageMaxHelper)
	}
	if payloadSize < 0 || payloadSize > wslStageMaxSize {
		return "", fmt.Errorf("native WSL payload size is outside the finite upload limit")
	}
	if timeout < time.Millisecond || timeout > nativeWSLUploadMaxWait {
		return "", fmt.Errorf("native WSL upload timeout must be between 1ms and %s (Windows wait limit)", nativeWSLUploadMaxWait)
	}
	if !shell.valid() {
		return "", fmt.Errorf("native WSL upload requires an established shell route")
	}
	command := wslStagePowerShellCommand(nativeWSLUploadScript(len(installer), payloadSize, timeout), shell)
	if len(command) >= wslStageLauncherCommandLimit {
		return "", fmt.Errorf("native WSL upload launcher exceeds its command budget")
	}
	return command, nil
}

func nativeWSLUploadScript(installerSize int, payloadSize int64, timeout time.Duration) string {
	args := append(nativeWSLPOSIXArgs(wslPOSIXHelperBootstrap), "sh", strconv.Itoa(installerSize))
	prefix := nativeWSLExecArguments(args)
	return strings.NewReplacer(
		"@PREFIX@", psQuote(prefix+" "),
		"@PAYLOAD@", strconv.FormatInt(payloadSize, 10),
		"@TOTAL@", strconv.FormatInt(int64(installerSize)+payloadSize, 10),
		"@TIMEOUT@", strconv.FormatInt(timeout.Milliseconds(), 10),
	).Replace(nativeWSLUploadTemplate)
}

// The two stream-opening lines are deliberately distinct: Win32-OpenSSH stdin
// requires an overlapped handle, while the WSL pipe uses the existing unbuffered
// synchronous-handle contract. Both transfers are awaited with the same deadline.
const nativeWSLUploadTemplate = `$ErrorActionPreference='Stop'
$p=$null;$src=$null;$dst=$null;$h=$null;$task=$null;$code=125
$clock=[Diagnostics.Stopwatch]::StartNew()
$cancel=[Threading.CancellationTokenSource]::new()
function Left {
  $n=@TIMEOUT@L-$clock.ElapsedMilliseconds
  if($n -le 0){throw 'WSL upload timed out'}
  return [int]$n
}
function Await($t) {
  if(!$t.Wait((Left))){throw 'WSL upload timed out'}
  return $t.GetAwaiter().GetResult()
}
try {
  if(!('Cbx.SshStdin' -as [type])){Add-Type -Name SshStdin -Namespace Cbx -MemberDefinition '[DllImport("kernel32.dll")]public static extern IntPtr GetStdHandle(int n);'}
  $h=[Microsoft.Win32.SafeHandles.SafeFileHandle]::new([Cbx.SshStdin]::GetStdHandle(-10),$false)
  $src=[IO.FileStream]::new($h,[IO.FileAccess]::Read,1,$true)
  $i=[Diagnostics.ProcessStartInfo]::new('wsl.exe')
  $i.UseShellExecute=$false;$i.RedirectStandardInput=$true
  $enc=[Console]::InputEncoding
  if($PSVersionTable.PSEdition -eq 'Core'){$enc=[Text.UTF8Encoding]::new($false);$i.StandardInputEncoding=$enc}
  $pre=$enc.GetPreamble()
  if([BitConverter]::ToString($pre) -notin @('','EF-BB-BF')){throw 'WSL upload input encoding is unsupported'}
  $i.Arguments=@PREFIX@+$pre.Length+' @PAYLOAD@'
  $p=[Diagnostics.Process]::Start($i)
  $task=$p.StandardInput.FlushAsync();$null=Await $task
  $dst=[IO.FileStream]::new($p.StandardInput.BaseStream.SafeFileHandle,[IO.FileAccess]::Write,1,$false)
  $buffer=[byte[]]::new(65536);$remaining=@TOTAL@L
  while($remaining -gt 0){
    $task=$src.ReadAsync($buffer,0,[int][Math]::Min($buffer.Length,$remaining),$cancel.Token)
    $n=Await $task
    if(!$n){throw 'WSL upload ended before its finite frame'}
    $task=$dst.WriteAsync($buffer,0,$n,$cancel.Token);$null=Await $task
    $remaining-=$n
  }
  $dst.Dispose();$dst=$null
  if(!$p.WaitForExit((Left))){throw 'WSL upload timed out'}
  $code=$p.ExitCode
  $null=Left
} catch {
  $reason=$_.Exception.Message
  if($reason -eq 'WSL upload timed out'){$code=124}
  elseif($reason -ne 'WSL upload ended before its finite frame' -and $reason -ne 'WSL upload input encoding is unsupported'){$reason='WSL upload failed'}
  [Console]::Error.WriteLine($reason)
} finally {
  $cancel.Cancel()
  if($p){
    try {if(!$p.HasExited){$p.Kill();if(!$p.WaitForExit(1000)){throw 'termination'}}}
    catch {[Console]::Error.WriteLine('WSL upload launcher termination unconfirmed');$code=125}
  }
  if($task -and !$task.IsCompleted){try{$null=$task.Wait(1000)}catch{}}
  if(!$task -or $task.IsCompleted){
    if($dst){$dst.Dispose()};if($src){$src.Dispose()}
  } else {[Console]::Error.WriteLine('WSL upload pipe cancellation unconfirmed');$code=125}
  if($h){$h.Dispose()};if($p){$p.Dispose()};$cancel.Dispose()
}
exit $code`
