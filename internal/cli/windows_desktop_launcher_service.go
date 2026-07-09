package cli

func windowsDesktopLauncherServicePowerShell() string {
	return `
$desktopLauncherService = "CrabboxDesktopLauncher"
$desktopLauncherExe = Join-Path $base "desktop-launcher-service.exe"
$desktopLauncherRequests = Join-Path $base "desktop-launch-requests"
$desktopLauncherSource = @'
using System;
using System.ComponentModel;
using System.IO;
using System.Runtime.InteropServices;
using System.ServiceProcess;
using System.Text;
using System.Threading;

public sealed class CrabboxDesktopLauncherService : ServiceBase {
    private const string BaseDirectory = @"C:\ProgramData\crabbox";
    private const string RequestDirectory = @"C:\ProgramData\crabbox\desktop-launch-requests";
    private const uint TOKEN_ASSIGN_PRIMARY = 0x0001;
    private const uint TOKEN_DUPLICATE = 0x0002;
    private const uint TOKEN_QUERY = 0x0008;
    private const uint CREATE_NEW_PROCESS_GROUP = 0x00000200;
    private const uint CREATE_UNICODE_ENVIRONMENT = 0x00000400;
    private const int SecurityImpersonation = 2;
    private const int TokenPrimary = 1;
    private const int WTSActive = 0;
    private volatile bool stopping;
    private Thread worker;

    [StructLayout(LayoutKind.Sequential)]
    private struct WTS_SESSION_INFO {
        public int SessionID;
        public IntPtr WinStationName;
        public int State;
    }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    private struct STARTUPINFO {
        public int cb;
        public string lpReserved;
        public string lpDesktop;
        public string lpTitle;
        public int dwX;
        public int dwY;
        public int dwXSize;
        public int dwYSize;
        public int dwXCountChars;
        public int dwYCountChars;
        public int dwFillAttribute;
        public int dwFlags;
        public short wShowWindow;
        public short cbReserved2;
        public IntPtr lpReserved2;
        public IntPtr hStdInput;
        public IntPtr hStdOutput;
        public IntPtr hStdError;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct PROCESS_INFORMATION {
        public IntPtr hProcess;
        public IntPtr hThread;
        public int dwProcessId;
        public int dwThreadId;
    }

    [DllImport("wtsapi32.dll", SetLastError = true)]
    private static extern bool WTSEnumerateSessions(IntPtr server, int reserved, int version, out IntPtr sessions, out int count);
    [DllImport("wtsapi32.dll")]
    private static extern void WTSFreeMemory(IntPtr memory);
    [DllImport("kernel32.dll")]
    private static extern uint WTSGetActiveConsoleSessionId();
    [DllImport("wtsapi32.dll", SetLastError = true)]
    private static extern bool WTSQueryUserToken(uint sessionId, out IntPtr token);
    [DllImport("advapi32.dll", SetLastError = true)]
    private static extern bool DuplicateTokenEx(IntPtr token, uint access, IntPtr attributes, int impersonationLevel, int tokenType, out IntPtr primaryToken);
    [DllImport("userenv.dll", SetLastError = true)]
    private static extern bool CreateEnvironmentBlock(out IntPtr environment, IntPtr token, bool inherit);
    [DllImport("userenv.dll", SetLastError = true)]
    private static extern bool DestroyEnvironmentBlock(IntPtr environment);
    [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool CreateProcessAsUserW(IntPtr token, string applicationName, StringBuilder commandLine, IntPtr processAttributes, IntPtr threadAttributes, bool inheritHandles, uint creationFlags, IntPtr environment, string currentDirectory, ref STARTUPINFO startupInfo, out PROCESS_INFORMATION processInformation);
    [DllImport("kernel32.dll")]
    private static extern bool CloseHandle(IntPtr handle);

    public CrabboxDesktopLauncherService() {
        ServiceName = "CrabboxDesktopLauncher";
        CanStop = true;
        AutoLog = false;
    }

    protected override void OnStart(string[] args) {
        Directory.CreateDirectory(RequestDirectory);
        stopping = false;
        worker = new Thread(WorkLoop);
        worker.IsBackground = true;
        worker.Start();
    }

    protected override void OnStop() {
        stopping = true;
        if (worker != null) worker.Join(5000);
    }

    private void WorkLoop() {
        while (!stopping) {
            foreach (string request in Directory.GetFiles(RequestDirectory, "*.request")) {
                HandleRequest(request);
            }
            Thread.Sleep(100);
        }
    }

    private static void HandleRequest(string requestPath) {
        string resultPath = null;
        try {
            string[] lines = File.ReadAllLines(requestPath);
            if (lines.Length != 2) throw new InvalidDataException("invalid desktop launch request");
            string scriptPath = ValidatedLaunchPath(lines[0], ".ps1");
            resultPath = ValidatedLaunchPath(lines[1], ".result");
            string requestID = Path.GetFileNameWithoutExtension(requestPath);
            if (Path.GetFileNameWithoutExtension(scriptPath) != requestID || Path.GetFileNameWithoutExtension(resultPath) != requestID) {
                throw new InvalidDataException("desktop launch request paths do not share one identity");
            }
            LaunchInteractive(scriptPath, resultPath);
        } catch (Exception error) {
            if (resultPath != null) WriteError(resultPath, error.Message);
        } finally {
            try { File.Delete(requestPath); } catch { }
        }
    }

    private static string ValidatedLaunchPath(string path, string extension) {
        string full = Path.GetFullPath(path.Trim());
        string expectedRoot = Path.GetFullPath(BaseDirectory) + Path.DirectorySeparatorChar;
        if (!full.StartsWith(expectedRoot, StringComparison.OrdinalIgnoreCase) ||
            !string.Equals(Path.GetDirectoryName(full), Path.GetFullPath(BaseDirectory), StringComparison.OrdinalIgnoreCase) ||
            !string.Equals(Path.GetExtension(full), extension, StringComparison.OrdinalIgnoreCase) ||
            !Path.GetFileName(full).StartsWith("desktop-launch-", StringComparison.OrdinalIgnoreCase)) {
            throw new InvalidDataException("desktop launch path is outside the managed directory");
        }
        return full;
    }

    private static int ActiveSessionId() {
        uint console = WTSGetActiveConsoleSessionId();
        int fallback = -1;
        IntPtr sessions;
        int count;
        if (!WTSEnumerateSessions(IntPtr.Zero, 0, 1, out sessions, out count)) {
            throw Win32("enumerate Windows sessions");
        }
        try {
            int size = Marshal.SizeOf(typeof(WTS_SESSION_INFO));
            for (int index = 0; index < count; index++) {
                WTS_SESSION_INFO session = (WTS_SESSION_INFO)Marshal.PtrToStructure(IntPtr.Add(sessions, index * size), typeof(WTS_SESSION_INFO));
                if (session.State != WTSActive) continue;
                if (session.SessionID == (int)console) return session.SessionID;
                if (fallback < 0) fallback = session.SessionID;
            }
        } finally {
            WTSFreeMemory(sessions);
        }
        if (fallback >= 0) return fallback;
        throw new InvalidOperationException("no active interactive Windows session");
    }

    private static void LaunchInteractive(string scriptPath, string resultPath) {
        int sessionId = ActiveSessionId();
        IntPtr sessionToken = IntPtr.Zero;
        IntPtr primaryToken = IntPtr.Zero;
        IntPtr environment = IntPtr.Zero;
        PROCESS_INFORMATION process = new PROCESS_INFORMATION();
        try {
            if (!WTSQueryUserToken((uint)sessionId, out sessionToken)) throw Win32("query active session token");
            uint access = TOKEN_ASSIGN_PRIMARY | TOKEN_DUPLICATE | TOKEN_QUERY;
            if (!DuplicateTokenEx(sessionToken, access, IntPtr.Zero, SecurityImpersonation, TokenPrimary, out primaryToken)) {
                throw Win32("duplicate active session token");
            }
            if (!CreateEnvironmentBlock(out environment, primaryToken, false)) throw Win32("create active session environment");
            string application = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.Windows), @"System32\WindowsPowerShell\v1.0\powershell.exe");
            string command = "\"" + application + "\" -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File \"" + scriptPath + "\" -ResultPath \"" + resultPath + "\"";
            STARTUPINFO startup = new STARTUPINFO();
            startup.cb = Marshal.SizeOf(typeof(STARTUPINFO));
            startup.lpDesktop = @"winsta0\default";
            uint flags = CREATE_NEW_PROCESS_GROUP | CREATE_UNICODE_ENVIRONMENT;
            if (!CreateProcessAsUserW(primaryToken, application, new StringBuilder(command), IntPtr.Zero, IntPtr.Zero, false, flags, environment, BaseDirectory, ref startup, out process)) {
                throw Win32("launch in active interactive session");
            }
        } finally {
            if (process.hThread != IntPtr.Zero) CloseHandle(process.hThread);
            if (process.hProcess != IntPtr.Zero) CloseHandle(process.hProcess);
            if (environment != IntPtr.Zero) DestroyEnvironmentBlock(environment);
            if (primaryToken != IntPtr.Zero) CloseHandle(primaryToken);
            if (sessionToken != IntPtr.Zero) CloseHandle(sessionToken);
        }
    }

    private static Win32Exception Win32(string operation) {
        int error = Marshal.GetLastWin32Error();
        return new Win32Exception(error, operation + " (win32=" + error + ")");
    }

    private static void WriteError(string resultPath, string message) {
        string encoded = Convert.ToBase64String(Encoding.UTF8.GetBytes(message));
        string temporary = resultPath + ".tmp-" + Guid.NewGuid().ToString("N");
        File.WriteAllText(temporary, "CRABBOX_DESKTOP_ERROR message=" + encoded, Encoding.ASCII);
        File.Move(temporary, resultPath);
    }

    public static void Main() {
        ServiceBase.Run(new CrabboxDesktopLauncherService());
    }
}
'@
$existingDesktopLauncher = Get-Service -Name $desktopLauncherService -ErrorAction SilentlyContinue
if ($existingDesktopLauncher) {
  Stop-Service -Name $desktopLauncherService -Force -ErrorAction SilentlyContinue
  $existingDesktopLauncher.WaitForStatus([ServiceProcess.ServiceControllerStatus]::Stopped, [TimeSpan]::FromSeconds(15))
}
Remove-Item -Force -LiteralPath $desktopLauncherExe -ErrorAction SilentlyContinue
Add-Type -TypeDefinition $desktopLauncherSource -Language CSharp -OutputAssembly $desktopLauncherExe -OutputType ConsoleApplication -ReferencedAssemblies "System.ServiceProcess.dll"
New-Item -ItemType Directory -Force -Path $desktopLauncherRequests | Out-Null
foreach ($desktopLauncherPath in @($desktopLauncherExe, $desktopLauncherRequests)) {
  icacls.exe $desktopLauncherPath /inheritance:r /grant "*S-1-5-18:F" /grant "*S-1-5-32-544:F" | Out-Null
}
if (-not $existingDesktopLauncher) {
  New-Service -Name $desktopLauncherService -BinaryPathName ('"' + $desktopLauncherExe + '"') -StartupType Automatic | Out-Null
} else {
  Set-Service -Name $desktopLauncherService -StartupType Automatic
}
Start-Service -Name $desktopLauncherService
if ((Get-Service -Name $desktopLauncherService).Status -ne "Running") { throw "desktop launcher service failed to start" }
`
}
