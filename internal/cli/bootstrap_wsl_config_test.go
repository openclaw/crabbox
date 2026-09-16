package cli

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestManagedWSLConfigBeforeLaunch(t *testing.T) {
	for _, test := range []struct {
		name, target, mode string
		desktop, browser   bool
		want               bool
	}{
		{"linux", targetLinux, "", false, false, false},
		{"native windows", targetWindows, windowsModeNormal, false, false, false},
		{"native desktop", targetWindows, windowsModeNormal, true, false, false},
		{"headless wsl2", targetWindows, windowsModeWSL2, false, false, true},
		{"wsl2 desktop", targetWindows, windowsModeWSL2, true, false, false},
		{"wsl2 browser", targetWindows, windowsModeWSL2, false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.TargetOS, cfg.WindowsMode, cfg.Desktop, cfg.Browser = test.target, test.mode, test.desktop, test.browser
			script := cloudInit(cfg, "ssh-ed25519 test")
			if test.target == targetWindows {
				script = WindowsBootstrapPowerShell(cfg, "ssh-ed25519 test")
			}
			if got := strings.Contains(script, "guiApplications=false"); got != test.want {
				t.Fatalf("headless WSL policy present=%t, want %t", got, test.want)
			}
			wsl := test.target == targetWindows && test.mode == windowsModeWSL2
			if got := strings.Contains(script, "instanceIdleTimeout=-1"); got != wsl {
				t.Fatalf("managed distro lifetime policy present=%t, want %t", got, wsl)
			}
			if wsl {
				write := strings.Index(script, "[IO.File]::WriteAllText($wslConfigPath")
				update := strings.Index(script, "wsl.exe --update")
				shutdown := strings.Index(script, "wsl.exe --shutdown")
				launch := strings.Index(script, "wsl.exe --set-default-version")
				if write < 0 || update <= write || shutdown <= update || launch <= shutdown ||
					!strings.Contains(script, "if ($wslConfigChanged)") {
					t.Fatal("WSL policy must precede installation and restart changed configurations before distro launch")
				}
			}
		})
	}
}

func TestManagedWSLConfigPreservesOtherSettings(t *testing.T) {
	powerShell, err := exec.LookPath("pwsh")
	if err != nil {
		powerShell, err = exec.LookPath("powershell.exe")
	}
	if err != nil {
		t.Skip("PowerShell is unavailable")
	}
	cfg := baseConfig()
	cfg.TargetOS, cfg.WindowsMode = targetWindows, windowsModeWSL2
	bootstrap := windowsWSL2BootstrapPowerShell(cfg)
	start, end := strings.Index(bootstrap, "function Set-CrabboxWSLConfigValue"), strings.Index(bootstrap, "$wslConfigPath =")
	if start < 0 || end <= start {
		t.Fatal("bootstrap does not configure the managed WSL runtime")
	}
	for _, test := range []struct{ name, section, setting, input, want string }{
		{"missing file", "wsl2", "guiApplications=false", "", "[wsl2]\nguiApplications=false"},
		{"already disabled", "wsl2", "guiApplications=false", "[wsl2]\nguiApplications=false\n", "[wsl2]\nguiApplications=false\n"},
		{"preserve memory", "wsl2", "guiApplications=false", "[wsl2]\nmemory=4GB\nguiApplications=true\n", "[wsl2]\nmemory=4GB\nguiApplications=false\n"},
		{"other section", "wsl2", "guiApplications=false", "[experimental]\nsparseVhd=true", "[experimental]\nsparseVhd=true\n[wsl2]\nguiApplications=false"},
		{"insert before next section", "wsl2", "guiApplications=false", "[wsl2]\nmemory=4GB\n[experimental]\nsparseVhd=true", "[wsl2]\nmemory=4GB\nguiApplications=false\n[experimental]\nsparseVhd=true"},
		{"case whitespace comments", "wsl2", "guiApplications=false", "; keep\r\n [WSL2] ; keep\r\n GUIAPPLICATIONS = true\r\nprocessors=2", "; keep\n [WSL2] ; keep\nguiApplications=false\nprocessors=2"},
		{"same key in other section", "wsl2", "guiApplications=false", "[other]\nguiApplications=true\n[wsl2]\nguiApplications=true", "[other]\nguiApplications=true\n[wsl2]\nguiApplications=false"},
		{"duplicate sections", "wsl2", "guiApplications=false", "[wsl2]\nmemory=4GB\n[wsl2]\nguiApplications=true", "[wsl2]\nmemory=4GB\nguiApplications=false\n[wsl2]\nguiApplications=false"},
		{"lifetime missing file", "general", "instanceIdleTimeout=-1", "", "[general]\ninstanceIdleTimeout=-1"},
		{"lifetime already disabled", "general", "instanceIdleTimeout=-1", "[general]\ninstanceIdleTimeout=-1\n", "[general]\ninstanceIdleTimeout=-1\n"},
		{"lifetime preserve VM policy", "general", "instanceIdleTimeout=-1", "[wsl2]\nvmIdleTimeout=60000\nguiApplications=true", "[wsl2]\nvmIdleTimeout=60000\nguiApplications=true\n[general]\ninstanceIdleTimeout=-1"},
		{"lifetime replace default", "general", "instanceIdleTimeout=-1", "[general]\ninstanceIdleTimeout=15000\n[experimental]\nsparseVhd=true", "[general]\ninstanceIdleTimeout=-1\n[experimental]\nsparseVhd=true"},
		{"lifetime insert before next section", "general", "instanceIdleTimeout=-1", "[general]\ndistributionInstallPath=C:\\distros\n[wsl2]\nmemory=4GB", "[general]\ndistributionInstallPath=C:\\distros\ninstanceIdleTimeout=-1\n[wsl2]\nmemory=4GB"},
		{"lifetime case whitespace comments", "general", "instanceIdleTimeout=-1", "; keep\r\n [GENERAL] ; keep\r\n INSTANCEIDLETIMEOUT = 15000", "; keep\n [GENERAL] ; keep\ninstanceIdleTimeout=-1"},
		{"lifetime same key in other section", "general", "instanceIdleTimeout=-1", "[other]\ninstanceIdleTimeout=15000\n[general]\ninstanceIdleTimeout=1000", "[other]\ninstanceIdleTimeout=15000\n[general]\ninstanceIdleTimeout=-1"},
		{"lifetime duplicate sections and keys", "general", "instanceIdleTimeout=-1", "[general]\n[general]\ninstanceIdleTimeout=1000\ninstanceIdleTimeout=15000", "[general]\ninstanceIdleTimeout=-1\n[general]\ninstanceIdleTimeout=-1\ninstanceIdleTimeout=-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments := " " + psQuote(test.section) + " " + psQuote(test.setting)
			script := bootstrap[start:end] + "\n$first = Set-CrabboxWSLConfigValue " + psQuote(test.input) + arguments +
				"\n$second = Set-CrabboxWSLConfigValue $first" + arguments +
				"\n@{first=$first;second=$second} | ConvertTo-Json -Compress"
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, powerShell, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
			if err != nil {
				t.Fatalf("config conversion: %v: %s", err, out)
			}
			var got struct{ First, Second string }
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("invalid config result: %v: %s", err, out)
			}
			if got.First != test.want || got.Second != test.want {
				t.Fatalf("first=%q second=%q, want %q on both passes", got.First, got.Second, test.want)
			}
		})
	}
}
