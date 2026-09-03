package cli

import (
	"strconv"
	"strings"
	"testing"
)

func TestWindowsRuntimeBeforeManagedReadiness(t *testing.T) {
	for _, architecture := range []string{"amd64", "arm64"} {
		for _, desktop := range []bool{false, true} {
			cfg := baseConfig()
			cfg.TargetOS, cfg.WindowsMode, cfg.Architecture = targetWindows, windowsModeNormal, architecture
			cfg.Desktop = desktop
			cfg.Provider = "aws"
			aws := windowsBootstrapPowerShell(cfg, "ssh-ed25519 fixture")
			cfg.Provider = "azure"
			for name, script := range map[string]string{
				"aws-shared-core": aws,
				"azure-extension": azureWindowsBootstrapPowerShell(cfg, "ssh-ed25519 fixture"),
				"azure-snapshot":  azureWindowsSnapshotRehydratePowerShell(cfg, "ssh-ed25519 fixture"),
			} {
				t.Run(name+"/"+architecture+"/desktop="+strconv.FormatBool(desktop), func(t *testing.T) {
					assertWindowsRuntimeBeforeCore(t, script)
					call := strings.Index(script, "\nEnsure-CrabboxWindowsRuntime\n")
					clear := strings.Index(script, "Remove-Item -LiteralPath $setupCompletePath -Force -ErrorAction Stop")
					if clear < 0 || call < clear || strings.Count(script, "\nEnsure-CrabboxWindowsRuntime\n") != 1 {
						t.Fatal("runtime must run once after clearing stale readiness")
					}
					ready := strings.Index(script, "Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath")
					if ready >= 0 && ready < call {
						t.Fatal("readiness precedes runtime verification")
					}
					if ready < 0 && !(name == "azure-extension" && desktop) {
						t.Fatal("expected readiness write after runtime")
					}
					if (!desktop || name == "azure-extension") && strings.Contains(script, "Restart-Computer") {
						t.Fatal("native runtime bootstrap must leave reboots to the operator")
					}
				})
			}
		}
	}
}

func assertWindowsRuntimeBeforeCore(t *testing.T, script string) {
	t.Helper()
	previousEnd := -1
	for _, fragment := range []string{sharedWindowsRuntime(), sharedWindowsRuntimeGate(), sharedWindowsCore()} {
		index := strings.Index(script, fragment)
		if index < 0 || index < previousEnd || strings.Count(script, fragment) != 1 {
			t.Fatal("canonical runtime definitions, gate, and core must occur once in order")
		}
		previousEnd = index + len(fragment)
	}
	if strings.Count(script, "function Ensure-CrabboxWindowsRuntime {") != 1 || strings.Count(script, "\nEnsure-CrabboxWindowsRuntime\n") != 1 {
		t.Fatal("runtime helper definition and call must each occur once")
	}
}

func TestWindowsRuntimeDefaultNativeMode(t *testing.T) {
	cfg := baseConfig()
	cfg.TargetOS, cfg.WindowsMode = targetWindows, ""
	for name, render := range map[string]func(Config, string) string{
		"aws-shared-core": windowsBootstrapPowerShell,
		"azure-extension": azureWindowsBootstrapPowerShell,
		"azure-snapshot":  azureWindowsSnapshotRehydratePowerShell,
	} {
		t.Run(name, func(t *testing.T) {
			assertWindowsRuntimeBeforeCore(t, render(cfg, "ssh-ed25519 fixture"))
		})
	}
}

func assertWindowsRuntimeAbsent(t *testing.T, script string) {
	t.Helper()
	for _, unwanted := range []string{
		"CrabboxWindowsRuntime", "crabboxSetupWasComplete", "PendingBoot", "VC_redist",
		"Remove-Item -LiteralPath $setupCompletePath",
		windowsVCRuntimeX64URL, windowsVCRuntimeX64SHA256, windowsVCRuntimeARM64URL, windowsVCRuntimeARM64SHA256,
	} {
		if strings.Contains(script, unwanted) {
			t.Fatalf("WSL2 must omit native runtime prerequisite %q", unwanted)
		}
	}
}

func TestWindowsRuntimeAbsentFromWSL2(t *testing.T) {
	cfg := baseConfig()
	cfg.TargetOS, cfg.WindowsMode = targetWindows, windowsModeWSL2
	for name, render := range map[string]func(Config, string) string{
		"aws-shared-core": windowsBootstrapPowerShell,
		"azure-extension": azureWindowsBootstrapPowerShell,
		"azure-snapshot":  azureWindowsSnapshotRehydratePowerShell,
	} {
		t.Run(name, func(t *testing.T) {
			script := render(cfg, "ssh-ed25519 fixture")
			assertWindowsRuntimeAbsent(t, script)
			core := strings.Index(script, sharedWindowsCore())
			ready := strings.Index(script, "Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath")
			if core < 0 || ready <= core || strings.Count(script, sharedWindowsCore()) != 1 {
				t.Fatal("WSL2 must retain baseline core and final readiness")
			}
			if name == "aws-shared-core" {
				wsl := strings.Index(script, windowsWSL2BootstrapPowerShell(cfg))
				if wsl < core+len(sharedWindowsCore()) || !strings.Contains(script, "Restart-CrabboxBootstrap") {
					t.Fatal("WSL2 must retain its full bootstrap and reboot flow after core")
				}
			} else if strings.Contains(script, "Restart-Computer") {
				t.Fatal("Azure baseline bootstrap must not introduce a reboot")
			}
		})
	}
}
