package cli

import (
	"runtime"
	"strings"
)

// psModulePathEnv is the module search path Windows PowerShell consults.
const psModulePathEnv = "PSModulePath"

// localCommandChildEnv applies platform corrections to a local child's
// environment before the process is started.
func localCommandChildEnv(name string, env []string) []string {
	if runtime.GOOS != "windows" {
		return env
	}
	return windowsDesktopPowerShellChildEnv(name, env)
}

// windowsDesktopPowerShellChildEnv drops an inherited PSModulePath when name
// launches Windows PowerShell (Desktop 5.1).
//
// Desktop PowerShell locates its in-box modules through PSModulePath. A
// PowerShell 7 parent adds its Core module directories to the process value.
// Launching powershell.exe directly from pwsh is safe because pwsh restores the
// Desktop value for that child, but any intermediate process - a driver, an
// agent, or crabbox itself - passes the Core-polluted value straight through.
// Desktop PowerShell then finds the Core copy of an in-box module such as
// Microsoft.PowerShell.Security ahead of its own and cannot load it, so a
// host-side ConvertTo-SecureString silently yields no value and the credential
// that depends on it is never constructed.
//
// Passing no value is deliberate. Desktop PowerShell then rebuilds the module
// path from the machine and user registry values, which keeps the child
// independent of whichever shell launched crabbox rather than trusting, or
// guessing corrections to, an inherited value. pwsh is excluded because the
// Core module directories are its own.
func windowsDesktopPowerShellChildEnv(name string, env []string) []string {
	if !isWindowsDesktopPowerShellCommand(name) {
		return env
	}
	return childEnvironmentWithout(env, psModulePathEnv)
}

// isWindowsDesktopPowerShellCommand reports whether name launches Windows
// PowerShell rather than PowerShell 7, which owns the Core module path.
func isWindowsDesktopPowerShellCommand(name string) bool {
	base := strings.TrimSpace(name)
	if index := strings.LastIndexAny(base, `\/`); index >= 0 {
		base = base[index+1:]
	}
	base = strings.ToLower(strings.TrimSpace(base))
	return base == "powershell" || base == "powershell.exe"
}
