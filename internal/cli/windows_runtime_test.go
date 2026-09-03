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
					if !strings.Contains(script, sharedWindowsRuntime()) || !strings.Contains(script, sharedWindowsCore()) {
						t.Fatal("missing canonical runtime/core fragment")
					}
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
