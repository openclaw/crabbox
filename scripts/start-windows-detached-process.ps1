# Explicit lease-lifetime launch. Ordinary commands remain in OpenSSH's session job.
[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$FilePath,
  [string]$ArgumentList = "",
  [string]$WorkingDirectory = (Get-Location).Path
)
$ErrorActionPreference = "Stop"
$executable = (Get-Command -Name $FilePath -CommandType Application -ErrorAction Stop).Source
if (-not (Test-Path -LiteralPath $WorkingDirectory -PathType Container)) { throw "working directory is missing" }
if (-not ("Crabbox.DetachedProcess" -as [type])) {
  Add-Type -TypeDefinition @"
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Text;
namespace Crabbox {
  public static class DetachedProcess {
    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    struct StartupInfo {
      public int cb;
      public string reserved, desktop, title;
      public int x, y, xSize, ySize, xChars, yChars, fill, flags;
      public short show, reservedSize;
      public IntPtr reservedBytes, stdin, stdout, stderr;
    }
    [StructLayout(LayoutKind.Sequential)]
    struct ProcessInfo {
      public IntPtr process, thread;
      public int pid, tid;
    }
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    static extern bool CreateProcessW(string application, StringBuilder command,
      IntPtr processAttributes, IntPtr threadAttributes, bool inheritHandles,
      uint flags, IntPtr environment, string directory, ref StartupInfo startup,
      out ProcessInfo process);
    [DllImport("kernel32.dll")]
    static extern bool CloseHandle(IntPtr handle);
    public static int Start(string executable, string arguments, string directory) {
      var startup = new StartupInfo();
      startup.cb = Marshal.SizeOf(startup);
      startup.flags = 1; // STARTF_USESHOWWINDOW
      startup.show = 0; // SW_HIDE
      ProcessInfo process;
      // Use a private hidden console: Windows PowerShell needs valid console handles.
      const uint CREATE_BREAKAWAY_FROM_JOB = 0x01000000;
      const uint CREATE_NEW_CONSOLE = 0x00000010;
      var command = new StringBuilder("\"" + executable + "\" " + arguments);
      if (!CreateProcessW(executable, command, IntPtr.Zero, IntPtr.Zero, false,
          CREATE_BREAKAWAY_FROM_JOB | CREATE_NEW_CONSOLE, IntPtr.Zero, directory,
          ref startup, out process)) {
        throw new Win32Exception(Marshal.GetLastWin32Error());
      }
      CloseHandle(process.thread);
      CloseHandle(process.process);
      return process.pid;
    }
  }
}
"@
}
[Crabbox.DetachedProcess]::Start($executable, $ArgumentList, $WorkingDirectory)
