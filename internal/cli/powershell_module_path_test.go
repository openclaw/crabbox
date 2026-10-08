package cli

import (
	"slices"
	"testing"
)

func TestIsWindowsDesktopPowerShellCommand(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{name: "powershell", want: true},
		{name: "powershell.exe", want: true},
		{name: "POWERSHELL.EXE", want: true},
		{name: "  powershell.exe  ", want: true},
		{name: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, want: true},
		{name: "C:/Windows/System32/WindowsPowerShell/v1.0/powershell.exe", want: true},
		{name: "pwsh", want: false},
		{name: "pwsh.exe", want: false},
		{name: `C:\Program Files\PowerShell\7\pwsh.exe`, want: false},
		{name: "powershell_ise.exe", want: false},
		{name: "notpowershell.exe", want: false},
		{name: "", want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isWindowsDesktopPowerShellCommand(testCase.name); got != testCase.want {
				t.Fatalf("isWindowsDesktopPowerShellCommand(%q) = %v, want %v", testCase.name, got, testCase.want)
			}
		})
	}
}

func TestWindowsDesktopPowerShellChildEnvDropsInheritedModulePath(t *testing.T) {
	// Windows environment names are case-insensitive, so an inherited value
	// spelled any way must be dropped for Desktop PowerShell.
	env := []string{
		`PSModulePath=C:\Program Files\PowerShell\7\Modules;C:\Program Files\WindowsPowerShell\Modules`,
		"PATH=C:\\Windows",
		"_CRABBOX_GP=secret",
	}
	got := windowsDesktopPowerShellChildEnv("powershell.exe", env)
	want := []string{"PATH=C:\\Windows", "_CRABBOX_GP=secret"}
	if !slices.Equal(got, want) {
		t.Fatalf("child env = %q, want %q", got, want)
	}

	mixedCase := []string{"psmodulepath=core-only", "PATH=C:\\Windows"}
	if got := windowsDesktopPowerShellChildEnv("powershell", mixedCase); !slices.Equal(got, []string{"PATH=C:\\Windows"}) {
		t.Fatalf("mixed-case child env = %q, want PATH only", got)
	}
}

func TestWindowsDesktopPowerShellChildEnvKeepsUnrelatedCommands(t *testing.T) {
	env := []string{`PSModulePath=C:\Program Files\PowerShell\7\Modules`, "PATH=C:\\Windows"}
	// PowerShell 7 owns the Core module directories, so its own value stands.
	for _, name := range []string{"pwsh.exe", `C:\Program Files\PowerShell\7\pwsh.exe`, "ssh.exe"} {
		if got := windowsDesktopPowerShellChildEnv(name, env); !slices.Equal(got, env) {
			t.Fatalf("child env for %q = %q, want it unchanged", name, got)
		}
	}
}

func TestWindowsDesktopPowerShellChildEnvPreservesDriveEntries(t *testing.T) {
	// Windows exposes per-drive working directories as nameless "=C:" entries.
	env := []string{`=C:=C:\work`, `PSModulePath=C:\Program Files\PowerShell\7\Modules`}
	if got := windowsDesktopPowerShellChildEnv("powershell.exe", env); !slices.Equal(got, []string{`=C:=C:\work`}) {
		t.Fatalf("child env = %q, want the drive entry preserved", got)
	}
}
