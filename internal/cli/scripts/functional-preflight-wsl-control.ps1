$ErrorActionPreference='Stop'
$b=[Convert]::FromBase64String('@PROGRAM@')
$e=[Text.UTF8Encoding]::new($false)
$o=[Console]::InputEncoding
$p=$null
try {
[Console]::InputEncoding=$e
$i=[Diagnostics.ProcessStartInfo]::new('wsl.exe')
$i.UseShellExecute=$false
$i.RedirectStandardInput=$true
$i.Arguments='--exec bash -s'
if($PSVersionTable.PSEdition -eq 'Core'){$i.StandardInputEncoding=$e}
$p=[Diagnostics.Process]::Start($i)
if($p.StandardInput.Encoding.GetPreamble().Length){throw 'control input preamble'}
$p.StandardInput.Flush()
$p.StandardInput.BaseStream.Write($b,0,$b.Length)
$p.StandardInput.BaseStream.Flush()
$p.StandardInput.Close()
$p.WaitForExit()
$c=$p.ExitCode
} finally {
if($p){$p.Dispose()}
[Console]::InputEncoding=$o
}
exit $c
